/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// clusterRowStack is a primary stack with the engine on public hosts and an in-cluster control DB.
func clusterRowStack(mut func(*oramav1alpha1.OramaCloudV1)) *oramav1alpha1.OramaCloudV1 {
	return engineOC(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.ControlDB = oramav1alpha1.ControlDBSpec{DataStoreSpec: oramav1alpha1.DataStoreSpec{Enabled: true, Mode: "in-cluster"}}
		o.Spec.Components.Engine.Writer.Host = "writer.orama.ing"
		o.Spec.Components.Engine.Reader.Host = "reader.orama.ing"
		if mut != nil {
			mut(o)
		}
	})
}

// clusterRowHarness is a reconciler over a fake client holding oc, its master key Secret and its
// control DB PostgresSchemas, every one at its current version in the given phase, plus objs.
func clusterRowHarness(t *testing.T, oc *oramav1alpha1.OramaCloudV1, masterKey, schemaPhase string, objs ...client.Object) (*OramaCloudV1Reconciler, client.Client) {
	t.Helper()
	schemas, err := controlDBSchemas(oc)
	if err != nil {
		t.Fatal(err)
	}
	for _, sc := range schemas {
		v := schemaVersion(sc.sql)
		objs = append(objs, &oramav1alpha1.PostgresSchema{
			ObjectMeta: metav1.ObjectMeta{Name: oc.Name + sc.suffix, Namespace: oc.Namespace, Generation: 1},
			Spec:       oramav1alpha1.PostgresSchemaSpec{Version: v},
			Status:     oramav1alpha1.PostgresSchemaStatus{Phase: schemaPhase, AppliedVersion: v, ObservedGeneration: 1},
		})
	}
	objs = append(objs, oc, secretOf(testMasterSecret, map[string]string{"MASTER_API_KEY": masterKey}))
	s := ocv1TestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &OramaCloudV1Reconciler{Client: cl, Scheme: s}, cl
}

func getClusterRowJob(t *testing.T, cl client.Client, oc *oramav1alpha1.OramaCloudV1) (*batchv1.Job, error) {
	t.Helper()
	job := &batchv1.Job{}
	err := cl.Get(context.Background(), client.ObjectKey{Namespace: oc.Namespace, Name: controlDBClusterRowJobName(oc)}, job)
	return job, err
}

func clusterRowReason(t *testing.T, r *OramaCloudV1Reconciler, oc *oramav1alpha1.OramaCloudV1) string {
	t.Helper()
	blocked, err := r.validateMasterAPIKey(context.Background(), oc)
	if err != nil {
		t.Fatal(err)
	}
	cond, err := r.controlDBClusterRowCondition(context.Background(), oc, blocked)
	if err != nil || cond == nil {
		t.Fatalf("ClusterRowReady: %+v, %v", cond, err)
	}
	return cond.Reason
}

// TestControlDBClusterRowJobWaitsForSchemaAndKey checks the Job is created only once the schema is
// applied and the master key is valid, with the public writer URL, the master key from
// spec.masterAPIKeySecretRef, and only where a stack inserts a row.
func TestControlDBClusterRowJobWaitsForSchemaAndKey(t *testing.T) {
	ctx := context.Background()
	oc := clusterRowStack(nil)

	r, cl := clusterRowHarness(t, oc, testMasterKey, "Applying")
	if err := r.reconcileControlDBClusterRow(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if _, err := getClusterRowJob(t, cl, oc); !apierrors.IsNotFound(err) {
		t.Fatalf("Job before the schema applied: %v", err)
	}
	if got := clusterRowReason(t, r, oc); got != "WaitingForSchema" {
		t.Fatalf("reason before the schema applied = %s", got)
	}

	// The row is written once, so a placeholder key must never reach it.
	r, cl = clusterRowHarness(t, oc, "dev-master-key", "Applied")
	if err := r.reconcileControlDBClusterRow(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if _, err := getClusterRowJob(t, cl, oc); !apierrors.IsNotFound(err) {
		t.Fatalf("Job with a placeholder master key: %v", err)
	}
	if got := clusterRowReason(t, r, oc); got != "WaitingForMasterAPIKey" {
		t.Fatalf("reason with a placeholder master key = %s", got)
	}

	r, cl = clusterRowHarness(t, oc, testMasterKey, "Applied")
	if err := r.reconcileControlDBClusterRow(ctx, oc); err != nil {
		t.Fatal(err)
	}
	job, err := getClusterRowJob(t, cl, oc)
	if err != nil {
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
		"CLUSTER_PREFIX":     "c1",
		"CLUSTER_WRITER_URL": "https://writer.orama.ing",
		"CLUSTER_READER_URL": "https://reader.orama.ing",
		"MASTER_API_KEY":     "secret:" + testMasterSecret + "/MASTER_API_KEY",
		"DATABASE_URL":       "secret:v1-controldb-app/uri",
	} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}
	if got := clusterRowReason(t, r, oc); got != "Inserting" {
		t.Fatalf("reason while the Job runs = %s", got)
	}

	// The row is written once, so it never records an in-cluster URL: without both public hosts the
	// operator creates no Job and says which host it waits for.
	for name, tc := range map[string]struct {
		mut     func(*oramav1alpha1.OramaCloudV1)
		missing []string
	}{
		"no writer host": {func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Writer.Host = "" },
			[]string{"spec.components.engine.writer.host"}},
		"no reader host": {func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Reader.Host = "" },
			[]string{"spec.components.engine.reader.host"}},
		"no hosts": {func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Components.Engine.Writer.Host, o.Spec.Components.Engine.Reader.Host = "", ""
		}, []string{"spec.components.engine.writer.host", "spec.components.engine.reader.host"}},
	} {
		t.Run(name, func(t *testing.T) {
			noHosts := clusterRowStack(tc.mut)
			r, cl := clusterRowHarness(t, noHosts, testMasterKey, "Applied")
			if err := r.reconcileControlDBClusterRow(ctx, noHosts); err != nil {
				t.Fatal(err)
			}
			if _, err := getClusterRowJob(t, cl, noHosts); !apierrors.IsNotFound(err) {
				t.Fatalf("Job without the public hosts: %v", err)
			}
			cond, err := r.controlDBClusterRowCondition(ctx, noHosts, nil)
			if err != nil {
				t.Fatal(err)
			}
			if cond.Reason != "WaitingForPublicHosts" {
				t.Fatalf("reason = %s, want WaitingForPublicHosts", cond.Reason)
			}
			for _, field := range tc.missing {
				if !strings.Contains(cond.Message, field) {
					t.Errorf("message %q does not name %s", cond.Message, field)
				}
			}
		})
	}

	// An external or disabled control DB, a read region, or a stack without the engine inserts no row.
	for name, mut := range map[string]func(*oramav1alpha1.OramaCloudV1){
		"external":    func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.ControlDB.Mode = "external" },
		"disabled":    func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.ControlDB.Enabled = false },
		"read region": func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Region.Role = "read" },
		"no engine":   func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Enabled = false },
	} {
		if controlDBClusterRowApplies(clusterRowStack(mut)) {
			t.Errorf("%s: inserts a cluster row", name)
		}
	}
}

// TestControlDBClusterRowRetriesAFailedJob checks the operator replaces a failed Job after
// clusterRowRetryDelay and not before, replaces an unfinished Job built from an older template at
// once, and never replaces a succeeded Job.
func TestControlDBClusterRowRetriesAFailedJob(t *testing.T) {
	ctx := context.Background()
	oc := clusterRowStack(nil)
	failedAt := func(ago time.Duration) *batchv1.Job {
		job := controlDBClusterRowJob(oc)
		job.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(oc, oramav1alpha1.GroupVersion.WithKind("OramaCloudV1"))}
		job.Status.Failed = clusterRowBackoffLimit + 1
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			LastTransitionTime: metav1.NewTime(time.Now().Add(-ago))}}
		return job
	}

	// Failed a minute ago: kept until the back-off has passed, reported as JobFailed.
	r, cl := clusterRowHarness(t, oc, testMasterKey, "Applied", failedAt(time.Minute))
	if err := r.reconcileControlDBClusterRow(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if job, err := getClusterRowJob(t, cl, oc); err != nil || !jobFailed(job) {
		t.Fatalf("failed Job replaced before its back-off: %v", err)
	}
	if got := clusterRowReason(t, r, oc); got != "JobFailed" {
		t.Fatalf("reason after a failure = %s", got)
	}
	if r.applyReadiness(ctx, oc, nil) {
		t.Fatal("a stack whose cluster-row Job failed reports itself settled, so it would not requeue")
	}

	// Failed longer ago than the back-off: deleted, then created again by the next reconcile.
	r, cl = clusterRowHarness(t, oc, testMasterKey, "Applied", failedAt(clusterRowRetryDelay+time.Second))
	if err := r.reconcileControlDBClusterRow(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if _, err := getClusterRowJob(t, cl, oc); !apierrors.IsNotFound(err) {
		t.Fatalf("failed Job not deleted after its back-off: %v", err)
	}
	if err := r.reconcileControlDBClusterRow(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if job, err := getClusterRowJob(t, cl, oc); err != nil || jobFailed(job) || job.Status.Failed != 0 {
		t.Fatalf("Job not created again after the failed one was deleted: %v", err)
	}

	// An unfinished Job from an older template (here, another image) is replaced at once.
	stale := controlDBClusterRowJob(oc)
	stale.OwnerReferences = failedAt(0).OwnerReferences
	stale.Annotations[clusterRowHashKey] = "older"
	r, cl = clusterRowHarness(t, oc, testMasterKey, "Applied", stale)
	if err := r.reconcileControlDBClusterRow(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if _, err := getClusterRowJob(t, cl, oc); !apierrors.IsNotFound(err) {
		t.Fatalf("unfinished Job from an older template not replaced: %v", err)
	}

	// A succeeded Job is the record and is never replaced, even from an older template.
	stale.Status.Succeeded = 1
	r, cl = clusterRowHarness(t, oc, testMasterKey, "Applied", stale)
	if err := r.reconcileControlDBClusterRow(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if _, err := getClusterRowJob(t, cl, oc); err != nil {
		t.Fatalf("succeeded Job replaced: %v", err)
	}
	if got := clusterRowReason(t, r, oc); got != "RowPresent" {
		t.Fatalf("reason after success = %s", got)
	}
	// True reports the Job, not the table: the message says it is not re-run after a restore, and
	// that a rotated master key does not reach the row.
	cond, err := r.controlDBClusterRowCondition(ctx, oc, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"reports the Job, not the table", "restore", "rotated spec.masterAPIKeySecretRef does not update it"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("RowPresent message %q does not say %q", cond.Message, want)
		}
	}
}

// TestPostgresClientImageOverride checks the postgres-client override reaches both psql Jobs, the
// control DB's PostgresSchema and the cluster-row Job, with the stack's pull secret, and that unset
// both keep the upstream default.
func TestPostgresClientImageOverride(t *testing.T) {
	ctx := context.Background()
	const pinned = "oramasearch/postgres-client@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for name, tc := range map[string]struct {
		mut   func(*oramav1alpha1.OramaCloudV1)
		image string
		pull  []corev1.LocalObjectReference
	}{
		"default": {image: "postgres:16.15-alpine"},
		"pinned": {
			mut: func(o *oramav1alpha1.OramaCloudV1) {
				o.Spec.Images.Overrides = map[string]string{"postgres-client": pinned}
				o.Spec.Images.PullSecretName = "dockerhub"
			},
			image: pinned,
			pull:  []corev1.LocalObjectReference{{Name: "dockerhub"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			oc := clusterRowStack(tc.mut)
			r, cl := clusterRowHarness(t, oc, testMasterKey, "Applied")
			if err := r.reconcileControlDBMigrations(ctx, oc); err != nil {
				t.Fatal(err)
			}
			ps := &oramav1alpha1.PostgresSchema{}
			if err := cl.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-controldb-schema"}, ps); err != nil {
				t.Fatal(err)
			}
			if ps.Spec.Image != tc.image || len(ps.Spec.ImagePullSecrets) != len(tc.pull) {
				t.Errorf("PostgresSchema image %q pull %v, want %q %v", ps.Spec.Image, ps.Spec.ImagePullSecrets, tc.image, tc.pull)
			}
			if spec := pgMigrationJobSpec(ps).Template.Spec; spec.Containers[0].Image != tc.image || len(spec.ImagePullSecrets) != len(tc.pull) {
				t.Errorf("migration Job image %q pull %v", spec.Containers[0].Image, spec.ImagePullSecrets)
			}
			spec := controlDBClusterRowJob(oc).Spec.Template.Spec
			if spec.Containers[0].Image != tc.image || len(spec.ImagePullSecrets) != len(tc.pull) {
				t.Errorf("cluster-row Job image %q pull %v", spec.Containers[0].Image, spec.ImagePullSecrets)
			}
		})
	}
}

// clusterRowTestDatabase creates a fresh database on the local Postgres and drops it when the test
// ends. Opt-in: CONTROLDB_TEST_POSTGRES=1, with CONTROLDB_TEST_POSTGRES_URI naming a role that may
// create databases (default postgres://postgres@127.0.0.1:5432/postgres). It drops only the database
// it created.
func clusterRowTestDatabase(t *testing.T) string {
	t.Helper()
	if os.Getenv("CONTROLDB_TEST_POSTGRES") != "1" {
		t.Skip("set CONTROLDB_TEST_POSTGRES=1 to run the cluster-row script against a local Postgres")
	}
	if _, err := exec.LookPath("psql"); err != nil {
		t.Fatal("psql is not on PATH")
	}
	admin := os.Getenv("CONTROLDB_TEST_POSTGRES_URI")
	if admin == "" {
		admin = "postgres://postgres@127.0.0.1:5432/postgres"
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "controldb_cluster_row_" + hex.EncodeToString(suffix)
	psqlQuery(t, admin, "CREATE DATABASE "+name)
	t.Cleanup(func() { psqlQuery(t, admin, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") })
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

func psqlQuery(t *testing.T, uri, sql string) string {
	t.Helper()
	out, err := exec.Command("psql", uri, "-X", "-q", "-tA", "-v", "ON_ERROR_STOP=1", "-c", sql).CombinedOutput()
	if err != nil {
		t.Fatalf("psql %q: %v\n%s", sql, err, out)
	}
	return strings.TrimSpace(string(out))
}

// runClusterRowScript runs the Job's script locally with the Job's env, the Secret-sourced values
// supplied directly, and fails the test if the output carries the master key.
func runClusterRowScript(t *testing.T, job *batchv1.Job, uri, masterKey string) (string, error) {
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
	if strings.Contains(string(out), masterKey) {
		t.Fatalf("script output carries the master key:\n%s", out)
	}
	return string(out), err
}

// TestControlDBClusterRowAgainstPostgres runs the Job's script against a real control DB with the
// vendored dashboard schema: a failed run changes nothing and its retry inserts the row; an empty
// table gets exactly the c1 row with the writer URL and the key; a second run, and a table holding
// any other row, are left unchanged.
func TestControlDBClusterRowAgainstPostgres(t *testing.T) {
	uri := clusterRowTestDatabase(t)
	job := controlDBClusterRowJob(clusterRowStack(nil))
	const key = "k3y'with\"quotes$and`ticks"
	const row = "select name, cluster_prefix, writer_url, reader_url, master_api_key, cluster_reader_replica_count, " +
		"cluster_writer_count, cluster_reader_regions::text, cluster_writer_regions::text from oramacore_clusters"

	// A run that fails (here the table does not exist yet, as after a restart mid-run) exits non-zero
	// and leaves nothing behind; the replacement Job's run then succeeds.
	if out, err := runClusterRowScript(t, job, uri, key); err == nil {
		t.Fatalf("run without the schema succeeded:\n%s", out)
	}
	if out, err := exec.Command("psql", uri, "-X", "-q", "-v", "ON_ERROR_STOP=1", "--single-transaction", "-c", dashboardSchemaSQL).CombinedOutput(); err != nil {
		t.Fatalf("apply dashboard schema: %v\n%s", err, out)
	}
	out, err := runClusterRowScript(t, job, uri, key)
	if err != nil || !strings.Contains(out, "inserted the c1 oramacore_clusters row (writer https://writer.orama.ing)") {
		t.Fatalf("retry on an empty table: %v\n%s", err, out)
	}
	want := `v1|c1|https://writer.orama.ing|https://reader.orama.ing|` + key + `|3|1|["us-east"]|["us-east"]`
	if got := psqlQuery(t, uri, row); got != want {
		t.Fatalf("row:\n got %s\nwant %s", got, want)
	}

	// A second reconcile's run changes nothing, whatever key it carries.
	out, err = runClusterRowScript(t, job, uri, "another-key")
	if err != nil || !strings.Contains(out, "already has a row; left unchanged") || psqlQuery(t, uri, row) != want {
		t.Fatalf("second run changed the table: %v\n%s", err, out)
	}

	// Any existing row, soft-deleted included, means no insert and no change.
	psqlQuery(t, uri, "delete from oramacore_clusters")
	psqlQuery(t, uri, `insert into oramacore_clusters (name, writer_url, reader_url, cluster_reader_replica_count,
		cluster_writer_count, cluster_reader_regions, cluster_writer_regions, master_api_key, cluster_prefix, deleted_at)
		values ('other', 'https://writer.example', 'https://reader.example', 3, 1, '[]', '[]', 'other-key', 'c1', now())`)
	before := psqlQuery(t, uri, "select row_to_json(c)::text from oramacore_clusters c")
	if _, err := runClusterRowScript(t, job, uri, key); err != nil {
		t.Fatal(err)
	}
	if after := psqlQuery(t, uri, "select row_to_json(c)::text from oramacore_clusters c"); after != before {
		t.Fatalf("existing row changed:\nbefore %s\n after %s", before, after)
	}
}
