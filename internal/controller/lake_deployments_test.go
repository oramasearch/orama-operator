// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"reflect"
	"regexp"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Deployments are on for every Lake (operator#557): lake gets the operator Zetta pair pointing at
// its own Zetta, with the token lake already holds for it, and a LAKE_PUBLIC_HOST to name its rows.
func TestEveryLakeGetsTheOperatorZettaPair(t *testing.T) {
	const url = "http://lake-dev-zetta-zetta.lake.svc.cluster.local:4800"
	assertPair := func(t *testing.T, env map[string]corev1.EnvVar) {
		t.Helper()
		if got := env["LAKE_OPERATOR_ZETTA_URL"]; got.Value != url || got.Value != env["LAKE_DATA_PLANE_URL"].Value {
			t.Errorf("LAKE_OPERATOR_ZETTA_URL = %+v, want %s", got, url)
		}
		token := env["LAKE_OPERATOR_ZETTA_TOKEN"]
		if token.Value != "" || token.ValueFrom == nil || token.ValueFrom.SecretKeyRef == nil ||
			token.ValueFrom.SecretKeyRef.Name != "lake-dev-zetta-zetta-admin" || token.ValueFrom.SecretKeyRef.Key != "token" ||
			token.ValueFrom.SecretKeyRef.Optional != nil {
			t.Errorf("LAKE_OPERATOR_ZETTA_TOKEN = %+v", token)
		}
		if !reflect.DeepEqual(token.ValueFrom, env["LAKE_DATA_PLANE_TOKEN"].ValueFrom) {
			t.Errorf("the operator token is not the data-plane token lake already holds")
		}
	}
	count := func(lake *oramav1alpha1.Lake, name string) int {
		n := 0
		for _, variable := range lakeEnv(lake, lakeSealingMaterial{}) {
			if variable.Name == name {
				n++
			}
		}
		return n
	}

	t.Run("a Lake with only spec.host", func(t *testing.T) {
		lake := bareLake()
		env := lakeEnvMap(lake)
		assertPair(t, env)
		if got := env["LAKE_PUBLIC_HOST"].Value; got != "lake-dev.nonprod.example" {
			t.Errorf("LAKE_PUBLIC_HOST = %q, want spec.host", got)
		}
		assertLakeRunsEveryFeature(t, lake)
	})

	t.Run("spec.env keeps its LAKE_PUBLIC_HOST, once", func(t *testing.T) {
		lake := bareLake()
		lake.Spec.Env = []corev1.EnvVar{{Name: "LAKE_PUBLIC_HOST", Value: "lake.nonprod.example"}}
		env := lakeEnvMap(lake)
		assertPair(t, env)
		if got := env["LAKE_PUBLIC_HOST"].Value; got != "lake.nonprod.example" || count(lake, "LAKE_PUBLIC_HOST") != 1 {
			t.Errorf("LAKE_PUBLIC_HOST = %q, appearing %d times", got, count(lake, "LAKE_PUBLIC_HOST"))
		}
	})

	t.Run("an empty LAKE_PUBLIC_HOST in spec.env is replaced by spec.host", func(t *testing.T) {
		lake := bareLake()
		lake.Spec.Env = []corev1.EnvVar{{Name: "LAKE_PUBLIC_HOST"}}
		if got := lakeEnvMap(lake)["LAKE_PUBLIC_HOST"].Value; got != "lake-dev.nonprod.example" || count(lake, "LAKE_PUBLIC_HOST") != 1 {
			t.Errorf("LAKE_PUBLIC_HOST = %q, appearing %d times", got, count(lake, "LAKE_PUBLIC_HOST"))
		}
	})

	t.Run("spec.env cannot point lake at another Zetta", func(t *testing.T) {
		lake := bareLake()
		lake.Spec.Env = []corev1.EnvVar{
			{Name: "LAKE_OPERATOR_ZETTA_URL", Value: "http://elsewhere:4800"},
			{Name: "LAKE_OPERATOR_ZETTA_TOKEN", Value: "literal"},
		}
		assertPair(t, lakeEnvMap(lake))
		if count(lake, "LAKE_OPERATOR_ZETTA_URL") != 1 || count(lake, "LAKE_OPERATOR_ZETTA_TOKEN") != 1 {
			t.Error("a reserved operator Zetta variable appears twice")
		}
	})

	t.Run("a Lake with no public host gets no pair and says why", func(t *testing.T) {
		lake := bareLake()
		lake.Spec.Host = ""
		env := lakeEnvMap(lake)
		for _, name := range []string{"LAKE_OPERATOR_ZETTA_URL", "LAKE_OPERATOR_ZETTA_TOKEN", "LAKE_PUBLIC_HOST"} {
			if _, found := env[name]; found {
				t.Errorf("%s given without a public host: lake would exit at startup", name)
			}
		}
		if condition := lakeFeaturesCondition(lake); condition.Status != metav1.ConditionFalse || condition.Reason != "NoPublicHost" {
			t.Errorf("FeaturesReady = %+v", condition)
		}
	})

	t.Run("a Lake with its Zetta off gets no pair and says why", func(t *testing.T) {
		lake := bareLake()
		off := false
		lake.Spec.Zetta.Enabled = &off
		env := lakeEnvMap(lake)
		if _, found := env["LAKE_OPERATOR_ZETTA_URL"]; found {
			t.Error("LAKE_OPERATOR_ZETTA_URL given without a Zetta")
		}
		if condition := lakeFeaturesCondition(lake); condition.Status != metav1.ConditionFalse || condition.Reason != "ZettaDisabled" {
			t.Errorf("FeaturesReady = %+v", condition)
		}
	})
}

// The Lake's own Zetta is the one operator-schema Zetta in its namespace, so the deployment
// control plane selects it, and lake's LAKE_OPERATOR_ZETTA_URL is the address the control plane
// writes to. A service Zetta in the same namespace stays plain and is not counted.
func TestTheLakeZettaIsTheOneOperatorSchemaZettaInItsNamespace(t *testing.T) {
	s := newLakeScheme(t)
	ctx := context.Background()
	lake := bareLake()
	lake.Name = "hello"
	plain := false
	service := &oramav1alpha1.OramaZettaCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "hello-svc-111111111111", Namespace: "lake",
			Labels: map[string]string{serviceLakeLabel: "hello", serviceIDLabel: "svc"}},
		Spec: oramav1alpha1.OramaZettaClusterSpec{Replicas: 1, ProvisionOperatorSchema: &plain},
	}
	workspace := &oramav1alpha1.OramaZettaCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "hello-ws-222222222222", Namespace: "hello-ws-222222222222"},
		Spec:       oramav1alpha1.OramaZettaClusterSpec{Replicas: 1, ProvisionOperatorSchema: &plain},
	}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(lake, service, workspace).
		WithStatusSubresource(lake, &oramav1alpha1.PostgresSchema{}, &oramav1alpha1.OramaZettaCluster{}).
		Build()
	applyLakeSchemaForWorkloadTest(t, c, s, lake)
	if _, err := (&LakeReconciler{Client: c, Scheme: s}).Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "hello"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var zettas oramav1alpha1.OramaZettaClusterList
	if err := c.List(ctx, &zettas, client.InNamespace("lake")); err != nil {
		t.Fatal(err)
	}
	var operatorSchema []string
	for _, zetta := range zettas.Items {
		if zetta.Spec.ProvisionOperatorSchema == nil || *zetta.Spec.ProvisionOperatorSchema {
			operatorSchema = append(operatorSchema, zetta.Name)
		}
	}
	if !reflect.DeepEqual(operatorSchema, []string{"lake-hello-zetta"}) {
		t.Fatalf("operator-schema Zettas in the Lake namespace = %v, want only the Lake's own", operatorSchema)
	}
	for _, other := range []*oramav1alpha1.OramaZettaCluster{service, workspace} {
		var current oramav1alpha1.OramaZettaCluster
		if err := c.Get(ctx, client.ObjectKeyFromObject(other), &current); err != nil {
			t.Fatal(err)
		}
		if current.Spec.ProvisionOperatorSchema == nil || *current.Spec.ProvisionOperatorSchema {
			t.Errorf("%s now provisions the operator schema", other.Name)
		}
	}

	// The Zetta controller mints the admin token; stand in for it.
	if err := c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "lake-hello-zetta-zetta-admin", Namespace: "lake"},
		Data:       map[string][]byte{"token": []byte("zetta-admin")},
	}); err != nil {
		t.Fatal(err)
	}
	var deploy corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake", Name: lakeDeploySecret("hello")}, &deploy); err != nil {
		t.Fatal(err)
	}
	target, failure := (&DeploymentControlPlane{Client: c}).authenticate(ctx, "Bearer "+string(deploy.Data["token"]))
	if failure != nil {
		t.Fatalf("the deployment control plane found no target: %+v", failure)
	}
	if target.Zetta.Name != "lake-hello-zetta" || target.ZettaToken != "zetta-admin" {
		t.Fatalf("target = %s with token %q", target.Zetta.Name, target.ZettaToken)
	}

	var deployment appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake", Name: "lake-hello"}, &deployment); err != nil {
		t.Fatal(err)
	}
	env := deployment.Spec.Template.Spec.Containers[0].Env
	url := findEnv(env, "LAKE_OPERATOR_ZETTA_URL")
	if url == nil || url.Value != target.BaseURL {
		t.Fatalf("LAKE_OPERATOR_ZETTA_URL = %+v, the control plane writes to %s", url, target.BaseURL)
	}
	token := findEnv(env, "LAKE_OPERATOR_ZETTA_TOKEN")
	if token == nil || token.ValueFrom == nil || token.ValueFrom.SecretKeyRef == nil ||
		token.ValueFrom.SecretKeyRef.Name != target.Zetta.Name+"-zetta-admin" {
		t.Fatalf("LAKE_OPERATOR_ZETTA_TOKEN = %+v", token)
	}
	if host := findEnv(env, "LAKE_PUBLIC_HOST"); host == nil || host.Value != "lake-dev.nonprod.example" {
		t.Fatalf("LAKE_PUBLIC_HOST = %+v", host)
	}
}

// The operator schema creates every table lake's platform sync reads, with lake's columns
// (lake include/lake/control/zetta_control.hpp), so a Lake's first sync succeeds on an empty
// control plane.
func TestTheOperatorSchemaHasLakesPlatformTables(t *testing.T) {
	want := map[string][]string{
		"environment_env": {"lake", "repository", "environment", "key", "value", "deleted", "set_by", "updated_at", "ordering_token"},
		"domains":         {"lake", "repository", "environment", "host", "tls", "status", "message", "deleted", "requested_by", "updated_at", "ordering_token"},
		"deployments":     nil, // lake reads it too; its columns are the operator's own (operator#124)
	}
	create := regexp.MustCompile(`^CREATE TABLE (\w+) \((.*)\)$`)
	found := map[string][]string{}
	for _, statement := range operatorZettaSchema {
		match := create.FindStringSubmatch(statement.sql)
		if statement.db != "orama_control" || match == nil {
			continue
		}
		var columns []string
		for _, column := range strings.Split(match[2], ", ") {
			columns = append(columns, strings.Fields(column)[0])
		}
		found[match[1]] = columns
	}
	for table, columns := range want {
		got, ok := found[table]
		if !ok {
			t.Errorf("orama_control.%s is not created", table)
			continue
		}
		if columns != nil && !reflect.DeepEqual(got, columns) {
			t.Errorf("orama_control.%s columns = %v, want %v", table, got, columns)
		}
	}
}

// Turning the operator schema on for a running Lake's Zetta does not restart it: nothing in the
// StatefulSet depends on the flag, and a dial-out-only slice derives no topology peers (the
// Lakes' Zettas are all dial-out-only), so the StatefulSet is rendered identically.
func TestTheOperatorSchemaLeavesTheLakeZettaStatefulSetUnchanged(t *testing.T) {
	r := &OramaZettaClusterReconciler{Scheme: zettaTestScheme(t)}
	render := func(operatorSchema bool) *appsv1.StatefulSet {
		z := &oramav1alpha1.OramaZettaCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "lake-dev-zetta", Namespace: "lake-dev", UID: "zetta-uid"},
			Spec: oramav1alpha1.OramaZettaClusterSpec{
				Replicas:                1,
				Image:                   lakeDefaultZettaImage,
				PullSecretName:          "oramasearch-dockerhub",
				Env:                     lakeDataPlaneZettaEnv(bareLake()),
				Reachability:            oramav1alpha1.ReachabilityDialOutOnly,
				ProvisionOperatorSchema: &operatorSchema,
			},
		}
		stsName, headless, _ := zettaNames(z)
		sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: stsName, Namespace: z.Namespace}}
		if err := r.mutateZettaStatefulSet(z, sts, headless, stsName, zettaLabels(z), "", nil); err != nil {
			t.Fatal(err)
		}
		return sts
	}
	if before, after := render(false), render(true); !reflect.DeepEqual(before.Spec, after.Spec) {
		t.Fatalf("the StatefulSet changes with provisionOperatorSchema:\nfalse %+v\ntrue  %+v", before.Spec, after.Spec)
	}
}
