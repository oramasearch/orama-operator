/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func clusterRowStack() *oramav1alpha1.OramaCloudV1 {
	return fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Writer.Host = "writer.orama.ing"
		o.Spec.Components.Engine.Reader.Host = "reader.orama.ing"
	})
}

// startTestPostgres runs a throwaway Postgres on a Unix socket and returns its connection URI.
// Skips when the Postgres binaries are not on PATH.
func startTestPostgres(t *testing.T) string {
	t.Helper()
	for _, bin := range []string{"initdb", "pg_ctl", "psql"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	dir, err := os.MkdirTemp("", "pg") // short: a Unix socket path is limited to ~100 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	data := filepath.Join(dir, "data")
	run := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
	}
	run("initdb", "-D", data, "-U", "postgres", "--auth=trust", "--no-sync")
	run("pg_ctl", "-D", data, "-w", "-l", filepath.Join(dir, "log"), "-o", "-k "+dir+" -c listen_addresses=", "start")
	t.Cleanup(func() { _ = exec.Command("pg_ctl", "-D", data, "-m", "immediate", "stop").Run() })
	return "postgresql://postgres@/postgres?host=" + dir
}

func psqlQuery(t *testing.T, uri, sql string) string {
	t.Helper()
	out, err := exec.Command("psql", uri, "-X", "-q", "-tA", "-v", "ON_ERROR_STOP=1", "-c", sql).CombinedOutput()
	if err != nil {
		t.Fatalf("psql %q: %v\n%s", sql, err, out)
	}
	return strings.TrimSpace(string(out))
}

// runClusterRowJob runs the Job's script locally with the Job's env, the Secret-sourced values
// supplied directly.
func runClusterRowJob(t *testing.T, job *batchv1.Job, uri, masterKey string) string {
	t.Helper()
	c := job.Spec.Template.Spec.Containers[0]
	env := []string{"PATH=" + os.Getenv("PATH"), "DATABASE_URL=" + uri, "MASTER_API_KEY=" + masterKey}
	for _, e := range c.Env {
		if e.ValueFrom == nil {
			env = append(env, e.Name+"="+e.Value)
		}
	}
	cmd := exec.Command(c.Command[0], c.Command[1:]...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cluster-row script: %v\n%s", err, out)
	}
	if strings.Contains(string(out), masterKey) {
		t.Fatalf("script output carries the master key:\n%s", out)
	}
	return string(out)
}

// TestControlDBClusterRowInsertsOnlyIntoEmptyTable runs the Job's script against a real control DB:
// a fresh one gets exactly the c1 row; a second run and a DB holding any other row are unchanged.
func TestControlDBClusterRowInsertsOnlyIntoEmptyTable(t *testing.T) {
	uri := startTestPostgres(t)
	if out, err := exec.Command("psql", uri, "-X", "-q", "-v", "ON_ERROR_STOP=1", "--single-transaction", "-c", dashboardSchemaSQL).CombinedOutput(); err != nil {
		t.Fatalf("apply dashboard schema: %v\n%s", err, out)
	}
	job := controlDBClusterRowJob(clusterRowStack())
	const key = "k3y'with\"quotes$and`ticks"
	const row = "select name, cluster_prefix, writer_url, reader_url, master_api_key, cluster_reader_replica_count, " +
		"cluster_writer_count, cluster_reader_regions::text, cluster_writer_regions::text from oramacore_clusters"

	out := runClusterRowJob(t, job, uri, key)
	if !strings.Contains(out, "inserted the c1 oramacore_clusters row") {
		t.Fatalf("fresh DB: no insert reported\n%s", out)
	}
	want := `v1|c1|https://writer.orama.ing|https://reader.orama.ing|` + key + `|3|1|["us-east"]|["us-east"]`
	if got := psqlQuery(t, uri, row); got != want {
		t.Fatalf("fresh DB row:\n got %s\nwant %s", got, want)
	}

	out = runClusterRowJob(t, job, uri, "another-key")
	if !strings.Contains(out, "already has a row; left unchanged") || psqlQuery(t, uri, row) != want {
		t.Fatalf("second run changed the table\n%s", out)
	}

	psqlQuery(t, uri, "delete from oramacore_clusters")
	psqlQuery(t, uri, `insert into oramacore_clusters (name, writer_url, reader_url, cluster_reader_replica_count,
		cluster_writer_count, cluster_reader_regions, cluster_writer_regions, master_api_key, cluster_prefix, deleted_at)
		values ('Atlantis', 'https://writer.atlantis.example', 'https://atlantis.example', 3, 1, '[]', '[]', 'prod1-key', 'c1', now())`)
	before := psqlQuery(t, uri, "select row_to_json(c)::text from oramacore_clusters c")
	runClusterRowJob(t, job, uri, key)
	if after := psqlQuery(t, uri, "select row_to_json(c)::text from oramacore_clusters c"); after != before {
		t.Fatalf("existing row changed:\nbefore %s\n after %s", before, after)
	}
}

// TestControlDBClusterRowJobWaitsForSchema checks the Job is created only once the control DB
// schema is applied, with the stack's URLs and master key Secret, and only where it applies.
func TestControlDBClusterRowJobWaitsForSchema(t *testing.T) {
	oc := clusterRowStack()
	ps := &oramav1alpha1.PostgresSchema{ObjectMeta: metav1.ObjectMeta{Name: oc.Name + "-controldb-schema", Namespace: oc.Namespace}}
	scheme := ocv1TestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(oc, ps).WithStatusSubresource(ps).Build()
	r := &OramaCloudV1Reconciler{Client: cl, Scheme: scheme}
	ctx := context.Background()
	key := client.ObjectKey{Namespace: oc.Namespace, Name: controlDBClusterRowJobName(oc)}

	if err := r.reconcileControlDBClusterRow(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, key, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Job before the schema applied: %v", err)
	}
	if cond, _ := r.controlDBClusterRowCondition(ctx, oc); cond == nil || cond.Reason != "WaitingForSchema" {
		t.Fatalf("condition before the schema applied: %+v", cond)
	}

	ps.Status.Phase = "Applied"
	if err := cl.Status().Update(ctx, ps); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileControlDBClusterRow(ctx, oc); err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{}
	if err := cl.Get(ctx, key, job); err != nil {
		t.Fatalf("Job after the schema applied: %v", err)
	}
	if !metav1.IsControlledBy(job, oc) {
		t.Fatal("Job is not owned by the CR")
	}
	env := map[string]string{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.ValueFrom != nil {
			env[e.Name] = "secret:" + e.ValueFrom.SecretKeyRef.Name + "/" + e.ValueFrom.SecretKeyRef.Key
		} else {
			env[e.Name] = e.Value
		}
	}
	for name, want := range map[string]string{
		"CLUSTER_WRITER_URL": "https://writer.orama.ing",
		"CLUSTER_READER_URL": "https://reader.orama.ing",
		"MASTER_API_KEY":     "secret:v1-master-api-key/MASTER_API_KEY",
		"DATABASE_URL":       "secret:v1-controldb-app/uri",
	} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}

	job.Status.Succeeded = 1
	if err := cl.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if cond, _ := r.controlDBClusterRowCondition(ctx, oc); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("condition after the Job succeeded: %+v", cond)
	}

	// No public hosts: the row points at the in-cluster Services.
	internal := fullStackOCV1(nil)
	env = map[string]string{}
	for _, e := range controlDBClusterRowEnv(internal) {
		env[e.Name] = e.Value
	}
	if env["CLUSTER_WRITER_URL"] != "http://v1-writer:8080" || env["CLUSTER_READER_URL"] != "http://v1-reader" {
		t.Errorf("in-cluster URLs = %q, %q", env["CLUSTER_WRITER_URL"], env["CLUSTER_READER_URL"])
	}

	// An external or replica control DB, or a stack without the engine, inserts no row.
	for name, mut := range map[string]func(*oramav1alpha1.OramaCloudV1){
		"external": func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.ControlDB.Mode = "external" },
		"replica": func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Region.Role, o.Spec.Components.ControlDB.Replicate = "read", true
		},
		"no engine": func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Enabled = false },
	} {
		if controlDBClusterRowApplies(fullStackOCV1(mut)) {
			t.Errorf("%s: inserts a cluster row", name)
		}
	}
}
