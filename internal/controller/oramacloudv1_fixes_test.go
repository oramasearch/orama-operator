/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func ocv1TestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := oramav1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newOCV1(mut func(*oramav1alpha1.OramaCloudV1)) *oramav1alpha1.OramaCloudV1 {
	oc := &oramav1alpha1.OramaCloudV1{
		ObjectMeta: metav1.ObjectMeta{Name: "v1", Namespace: "orama-system", UID: types.UID("uid-v1")},
		Spec: oramav1alpha1.OramaCloudV1Spec{
			Region: oramav1alpha1.RegionSpec{Name: "us-east", Role: "primary"},
		},
	}
	if mut != nil {
		mut(oc)
	}
	return oc
}

// TestBrokerTopologyStreamRetentionMatchesOramaCore is the regression for the live AKS 400: the
// operator MUST declare the oramacore-operations stream with the SAME x-max-length-bytes OramaCore
// uses when IT creates the stream on connect (5 GB). A mismatched (or absent) arg makes RabbitMQ
// reject the redeclare ("inequivalent arg 'x-max-length-bytes'"), which fails the whole topology
// reconcile before the reader/writer users are created — so the reader can never auth to the stream.
func TestBrokerTopologyStreamRetentionMatchesOramaCore(t *testing.T) {
	s := ocv1TestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
	oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Broker = oramav1alpha1.DataStoreSpec{Enabled: true, Mode: "in-cluster"}
	})
	if err := r.reconcileBrokerTopology(context.Background(), oc); err != nil {
		t.Fatalf("reconcileBrokerTopology: %v", err)
	}
	topo := &oramav1alpha1.RabbitMQTopology{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "v1-broker", Namespace: "orama-system"}, topo); err != nil {
		t.Fatalf("get RabbitMQTopology: %v", err)
	}
	if len(topo.Spec.Streams) != 1 {
		t.Fatalf("streams=%d, want 1", len(topo.Spec.Streams))
	}
	if got := topo.Spec.Streams[0].MaxLengthBytes; got != brokerOperationsStreamMaxBytes {
		t.Errorf("stream MaxLengthBytes=%d, want %d (must match OramaCore, else RabbitMQ 400s the redeclare and the reader can't auth)",
			got, brokerOperationsStreamMaxBytes)
	}
	if got := topo.Spec.Streams[0].LeaderLocator; got != brokerOperationsStreamLeaderLocator {
		t.Errorf("stream LeaderLocator=%q, want %q (OramaCore's second stream arg — a mismatch 400s the redeclare too)",
			got, brokerOperationsStreamLeaderLocator)
	}
}

// Direct helper calls bypass admission, but the dashboard must always leave schema migration
// to PostgresSchema, including when upgrading a Deployment that still has the old init container.
func TestDashboardNeverMigratesControlDB(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, mode := range []string{"", "in-cluster", "external"} {
			for _, upgrade := range []bool{false, true} {
				t.Run(fmt.Sprintf("enabled=%t/mode=%s/upgrade=%t", enabled, mode, upgrade), func(t *testing.T) {
					oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
						o.Name = "v1-dev"
						o.Spec.Components.Dashboard.Enabled = true
						o.Spec.MasterAPIKeySecretRef = &oramav1alpha1.MasterAPIKeySecretRef{Name: testMasterSecret}
						o.Spec.Components.ControlDB = oramav1alpha1.ControlDBSpec{DataStoreSpec: oramav1alpha1.DataStoreSpec{Enabled: enabled, Mode: mode}}
					})
					scheme := ocv1TestScheme(t)
					cl := fake.NewClientBuilder().WithScheme(scheme).Build()
					r := &OramaCloudV1Reconciler{Client: cl, Scheme: scheme}
					key := client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-dashboard"}
					if upgrade {
						old := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
						old.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "db-migrate", Image: "dashboard:old"}}
						if err := cl.Create(context.Background(), old); err != nil {
							t.Fatal(err)
						}
					}
					if err := r.reconcileDashboard(context.Background(), oc); err != nil {
						t.Fatal(err)
					}
					var dep appsv1.Deployment
					if err := cl.Get(context.Background(), key, &dep); err != nil {
						t.Fatal(err)
					}
					if len(dep.Spec.Template.Spec.InitContainers) != 0 {
						t.Fatal("dashboard still has init containers")
					}
					env := dep.Spec.Template.Spec.Containers[0].Env
					assertControlDBSecretEnv(t, env, "DATABASE_URL", "uri")
					assertControlDBSecretEnv(t, env, "POSTGRES_PASSWORD", "password")
				})
			}
		}
	}
}

func assertControlDBSecretEnv(t *testing.T, env []corev1.EnvVar, name, key string) {
	t.Helper()
	for _, v := range env {
		if v.Name != name {
			continue
		}
		if v.Value != "" || v.ValueFrom == nil || v.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("%s must come from a Secret", name)
		}
		ref := v.ValueFrom.SecretKeyRef
		if ref.Name != "v1-dev-controldb-app" || ref.Key != key {
			t.Fatalf("%s references %s/%s", name, ref.Name, ref.Key)
		}
		return
	}
	t.Fatalf("missing env %s", name)
}

// A pre-admission resource must be refused before any client access, especially before disabled
// teardown. Compare all control-DB resource kinds as well as exercising a nil client.
func TestControlDBRejectsExternalWithoutTouchingResources(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, credentials := range []bool{false, true} {
			for _, seeded := range []bool{false, true} {
				t.Run(fmt.Sprintf("enabled=%t/credentials=%t/seeded=%t", enabled, credentials, seeded), func(t *testing.T) {
					ctx := context.Background()
					oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
						o.Spec.Components.ControlDB = oramav1alpha1.ControlDBSpec{DataStoreSpec: oramav1alpha1.DataStoreSpec{Enabled: enabled, Mode: "external"}}
						if credentials {
							o.Spec.Components.ControlDB.Endpoint = "postgres.example.test:5432"
							o.Spec.Components.ControlDB.CredentialsSecretRef = &corev1.LocalObjectReference{Name: "external-creds"}
						}
					})
					const want = "controlDB mode=external is not supported: the control DB is always in-cluster"
					assertRejected := func(r *OramaCloudV1Reconciler) {
						t.Helper()
						if err := r.reconcileControlDB(ctx, oc); err == nil || err.Error() != want {
							t.Fatalf("reconcile error = %v, want %q", err, want)
						}
					}
					assertRejected(&OramaCloudV1Reconciler{}) // any client access would panic
					scheme := ocv1TestScheme(t)
					var objects []client.Object
					if seeded {
						meta := func(name string) metav1.ObjectMeta {
							return metav1.ObjectMeta{Name: oc.Name + name, Namespace: oc.Namespace, Labels: labelsFor(oc, "controldb")}
						}
						cluster := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{"instances": int64(2)}}}
						cluster.SetGroupVersionKind(cnpgClusterGVK)
						cluster.SetName(oc.Name + "-controldb")
						cluster.SetNamespace(oc.Namespace)
						objects = []client.Object{
							cluster,
							&oramav1alpha1.PostgresSchema{ObjectMeta: meta("-controldb-schema"), Spec: oramav1alpha1.PostgresSchemaSpec{Version: "existing"}},
							&corev1.ConfigMap{ObjectMeta: meta("-controldb-migrations"), Data: map[string]string{"schema.sql": "existing schema"}},
							&batchv1.Job{ObjectMeta: meta("-controldb-migrate")},
							&batchv1.Job{ObjectMeta: meta("-controldb-schema-migrate")},
							&corev1.Secret{ObjectMeta: meta("-controldb-app"), Data: map[string][]byte{"password": []byte("fixture-only")}},
							&corev1.PersistentVolumeClaim{ObjectMeta: meta("-controldb-1")},
						}
					}
					cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
					snapshot := func() []runtime.Object {
						t.Helper()
						clusters := &unstructured.UnstructuredList{}
						clusters.SetGroupVersionKind(cnpgClusterGVK.GroupVersion().WithKind("ClusterList"))
						lists := []client.ObjectList{clusters, &oramav1alpha1.PostgresSchemaList{}, &corev1.ConfigMapList{}, &batchv1.JobList{}, &corev1.SecretList{}, &corev1.PersistentVolumeClaimList{}}
						var result []runtime.Object
						for _, list := range lists {
							if err := cl.List(ctx, list, client.InNamespace(oc.Namespace)); err != nil {
								t.Fatal(err)
							}
							result = append(result, list.DeepCopyObject())
						}
						return result
					}
					before := snapshot()
					assertRejected(&OramaCloudV1Reconciler{Client: cl, Scheme: scheme})
					if !reflect.DeepEqual(before, snapshot()) {
						t.Fatal("external mode created, changed or deleted control-DB resources")
					}
				})
			}
		}
	}
}

func TestControlDBSupportedModes(t *testing.T) {
	for _, mode := range []string{"", "in-cluster"} {
		t.Run("mode="+mode, func(t *testing.T) {
			ctx := context.Background()
			oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
				o.Spec.Components.ControlDB = oramav1alpha1.ControlDBSpec{DataStoreSpec: oramav1alpha1.DataStoreSpec{Enabled: true, Mode: mode}}
			})
			scheme := ocv1TestScheme(t)
			cl := fake.NewClientBuilder().WithScheme(scheme).Build()
			r := &OramaCloudV1Reconciler{Client: cl, Scheme: scheme}
			if err := r.reconcileControlDB(ctx, oc); err != nil {
				t.Fatal(err)
			}
			cluster := newCNPGCluster(oc.Namespace, oc.Name+"-controldb")
			if err := cl.Get(ctx, client.ObjectKeyFromObject(cluster), cluster); err != nil {
				t.Fatal(err)
			}
			var ps oramav1alpha1.PostgresSchema
			if err := cl.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-controldb-schema"}, &ps); err != nil {
				t.Fatal(err)
			}
			if ps.Spec.ConnectionSecretRef.Name != controlDBAppSecret(oc) || ps.Spec.ConnectionSecretRef.Key != "uri" {
				t.Fatal("schema connection must use the CNPG URI")
			}
			oc.Spec.Components.ControlDB.Enabled = false
			if err := r.reconcileControlDB(ctx, oc); err != nil {
				t.Fatal(err)
			}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(cluster), cluster); !apierrors.IsNotFound(err) {
				t.Fatalf("disabled control DB cluster remains: %v", err)
			}
		})
	}
}

func TestAnalyticsProcessorRendersControlDBURIAndEscapesSecrets(t *testing.T) {
	ctx := context.Background()
	oc := newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Name = "v1-dev"
		o.Spec.Components.AnalyticsProcessor.Enabled = true
		o.Spec.MasterAPIKeySecretRef = &oramav1alpha1.MasterAPIKeySecretRef{Name: testMasterSecret}
	})
	scheme := ocv1TestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &OramaCloudV1Reconciler{Client: cl, Scheme: scheme}
	if err := r.reconcileAnalyticsProcessor(ctx, oc); err != nil {
		t.Fatal(err)
	}
	var dep appsv1.Deployment
	if err := cl.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-analytics-processor"}, &dep); err != nil {
		t.Fatal(err)
	}
	if len(dep.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatal("expected render init container")
	}
	render := dep.Spec.Template.Spec.InitContainers[0]
	assertControlDBSecretEnv(t, render.Env, "PG_URI", "uri")
	if len(render.Env) != 2 || render.Env[0].Name != masterAPIKeyEnvName {
		t.Fatal("render must take only the master key (spec.masterAPIKeySecretRef) and PG_URI as env overrides")
	}
	// The independent index-builder client uses the same helper and Secret.
	assertControlDBSecretEnv(t, r.indexBuilderEnv(ctx, oc), "DATABASE_URL", "uri")
	var cm corev1.ConfigMap
	if err := cl.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-analytics-processor-config"}, &cm); err != nil {
		t.Fatal(err)
	}
	template := cm.Data["config.yaml"]
	for _, placeholder := range []string{"__CLICKHOUSE_PASSWORD__", "__MASTER_API_KEY__", "__PG_URI__"} {
		if !strings.Contains(template, placeholder) {
			t.Fatalf("missing template placeholder %s", placeholder)
		}
	}
	if strings.Contains(template, "__PG_PASSWORD__") {
		t.Fatal("template still constructs a Postgres URL")
	}
	if len(render.Command) != 3 || render.Command[0] != "sh" || render.Command[1] != "-c" {
		t.Fatal("expected sh -c render command")
	}
	for _, name := range []string{"CLICKHOUSE_PASSWORD", "MASTER_API_KEY", "PG_URI"} {
		if !strings.Contains(render.Command[2], `$(esc "${`+name+`}")`) {
			t.Fatalf("%s bypasses escaping", name)
		}
	}
	values := map[string]string{
		"CLICKHOUSE_PASSWORD": `ch&pass#slash\end`,
		"MASTER_API_KEY":      `key&part#slash\end`,
		"PG_URI":              "postgresql://oramacore:p%23x@v1-controldb-rw:5432/oramacore?sslmode=prefer&application_name=render-proof",
	}
	for _, value := range values {
		if strings.Contains(template, value) {
			t.Fatal("Secret value leaked into template ConfigMap")
		}
	}
	dir := t.TempDir()
	input, output := filepath.Join(dir, "template.yaml"), filepath.Join(dir, "rendered.yaml")
	if err := os.WriteFile(input, []byte(template), 0600); err != nil {
		t.Fatal(err)
	}
	// Change only fixture paths; execute the generated command verbatim otherwise.
	command := strings.NewReplacer("/tmpl/config.yaml", input, "/etc/analytics-processor/config.yaml", output).Replace(render.Command[2])
	cmd := exec.Command(render.Command[0], render.Command[1], command)
	cmd.Env = os.Environ()
	for name, value := range values {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("render command failed: %v", err)
	}
	rendered, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), "__") {
		t.Fatal("render left a placeholder")
	}
	var config struct {
		Clickhouse struct{ Password string } `json:"clickhouse"`
		Oramacores []struct {
			APIKey string `json:"api_key"`
		} `json:"oramacores"`
		Postgres struct {
			URL string `json:"url"`
		} `json:"postgres"`
	}
	if err := yaml.Unmarshal(rendered, &config); err != nil {
		t.Fatal("rendered config is not valid YAML")
	}
	if config.Clickhouse.Password != values["CLICKHOUSE_PASSWORD"] {
		t.Error("ClickHouse password changed during render")
	}
	if len(config.Oramacores) != 1 || config.Oramacores[0].APIKey != values["MASTER_API_KEY"] {
		t.Error("master API key changed during render")
	}
	if config.Postgres.URL != values["PG_URI"] {
		t.Error("Postgres URI changed during render")
	}
}

// TestEnvoySidecarBypassesEntrypointChown is the regression for the reader's second blocker: the
// envoyproxy/envoy image's /docker-entrypoint.sh chowns /dev/stdout, which fails under the non-root
// hardened securityContext (#18) and exits the container. The operator must run envoy directly (its
// own Command) so the entrypoint's chown never runs.
func TestEnvoySidecarBypassesEntrypointChown(t *testing.T) {
	c := envoySidecar()
	if len(c.Command) == 0 || c.Command[0] != "envoy" {
		t.Errorf("envoy sidecar Command=%v, want it to exec `envoy` directly (bypass /docker-entrypoint.sh, whose chown fails as non-root)", c.Command)
	}
}
