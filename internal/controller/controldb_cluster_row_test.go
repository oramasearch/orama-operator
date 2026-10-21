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
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

// testClusterRowID is a c1 row id as the cluster-row Job reports it.
const testClusterRowID = "0f6b8a4e-3c1d-4e2a-9b7f-5d2c1a0e9f88"

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

	// A succeeded Job that recorded its row id is the record and is never replaced, even from an
	// older template (TestClusterRowIDReachesTheDashboard covers one that recorded none).
	stale.Status.Succeeded = 1
	stale.Annotations[clusterRowIDKey] = testClusterRowID
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
		"default": {image: "postgres:16-alpine"},
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
// supplied directly, and fails the test if the output carries the master key. The termination
// message goes to a temporary file instead of the kubelet's path; its content is the second result.
func runClusterRowScript(t *testing.T, job *batchv1.Job, uri, masterKey string) (string, string, error) {
	t.Helper()
	c := job.Spec.Template.Spec.Containers[0]
	termination := filepath.Join(t.TempDir(), "termination-log")
	env := []string{"PATH=" + os.Getenv("PATH"), "DATABASE_URL=" + uri, "MASTER_API_KEY=" + masterKey}
	for _, e := range c.Env {
		if e.ValueFrom == nil {
			env = append(env, e.Name+"="+e.Value)
		}
	}
	args := slices.Clone(c.Command[1:])
	args[len(args)-1] = strings.ReplaceAll(args[len(args)-1], clusterRowTerminationPath, termination)
	cmd := exec.Command(c.Command[0], args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if strings.Contains(string(out), masterKey) {
		t.Fatalf("script output carries the master key:\n%s", out)
	}
	msg, _ := os.ReadFile(termination) // absent when the script failed before writing it
	return string(out), string(msg), err
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
	if out, _, err := runClusterRowScript(t, job, uri, key); err == nil {
		t.Fatalf("run without the schema succeeded:\n%s", out)
	}
	if out, err := exec.Command("psql", uri, "-X", "-q", "-v", "ON_ERROR_STOP=1", "--single-transaction", "-c", dashboardSchemaSQL).CombinedOutput(); err != nil {
		t.Fatalf("apply dashboard schema: %v\n%s", err, out)
	}
	out, rowID, err := runClusterRowScript(t, job, uri, key)
	if err != nil || !strings.Contains(out, "inserted the c1 oramacore_clusters row (writer https://writer.orama.ing)") {
		t.Fatalf("retry on an empty table: %v\n%s", err, out)
	}
	// The termination message is the inserted row's id, which the dashboard is given (#572).
	if want := psqlQuery(t, uri, "select id from oramacore_clusters"); rowID != want || !clusterRowIDPattern.MatchString(rowID) {
		t.Fatalf("termination message %q, want the row id %s", rowID, want)
	}
	want := `v1|c1|https://writer.orama.ing|https://reader.orama.ing|` + key + `|3|1|["us-east"]|["us-east"]`
	if got := psqlQuery(t, uri, row); got != want {
		t.Fatalf("row:\n got %s\nwant %s", got, want)
	}

	// A second reconcile's run changes nothing, whatever key it carries.
	out, again, err := runClusterRowScript(t, job, uri, "another-key")
	if err != nil || !strings.Contains(out, "already has a row; left unchanged") || psqlQuery(t, uri, row) != want {
		t.Fatalf("second run changed the table: %v\n%s", err, out)
	}
	if again != rowID {
		t.Fatalf("second run reported %q, want the row it found, %s", again, rowID)
	}

	// Any existing row, soft-deleted included, means no insert and no change.
	psqlQuery(t, uri, "delete from oramacore_clusters")
	psqlQuery(t, uri, `insert into oramacore_clusters (name, writer_url, reader_url, cluster_reader_replica_count,
		cluster_writer_count, cluster_reader_regions, cluster_writer_regions, master_api_key, cluster_prefix, deleted_at)
		values ('other', 'https://writer.example', 'https://reader.example', 3, 1, '[]', '[]', 'other-key', 'c1', now())`)
	before := psqlQuery(t, uri, "select row_to_json(c)::text from oramacore_clusters c")
	_, none, err := runClusterRowScript(t, job, uri, key)
	if err != nil {
		t.Fatal(err)
	}
	if none != clusterRowNone {
		t.Fatalf("with only a soft-deleted c1 row the script reported %q, want %s", none, clusterRowNone)
	}
	if after := psqlQuery(t, uri, "select row_to_json(c)::text from oramacore_clusters c"); after != before {
		t.Fatalf("existing row changed:\nbefore %s\n after %s", before, after)
	}
}

// clusterRowPod is a pod of job that ended with the given phase and termination message.
func clusterRowPod(job *batchv1.Job, name string, phase corev1.PodPhase, message string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: job.Namespace, Labels: map[string]string{"job-name": job.Name},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID}}},
		Status: corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{{Name: "cluster-row",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: message}}}}},
	}
}

// TestClusterRowIDReachesTheDashboard checks the dashboard's derived values (#572): the Job's
// reported c1 row id is recorded on the succeeded Job and given to the dashboard as
// ORAMACORE_PLACEMENT_CLUSTER_ID, NEXT_PUBLIC_APP_URL follows the dashboard's own host, and
// REDIS_CACHE_URL names the shim (#562). A succeeded Job that reported no id is run again; one that
// found no live c1 row gives the dashboard no placement, so it uses the newest row.
func TestClusterRowIDReachesTheDashboard(t *testing.T) {
	ctx := context.Background()
	oc := clusterRowStack(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Cache = oramav1alpha1.DataStoreSpec{Enabled: true, Mode: "in-cluster"}
		o.Spec.Components.Dashboard = oramav1alpha1.DashboardSpec{ToggleWithHost: oramav1alpha1.ToggleWithHost{Enabled: true, Host: "app-dev.oramadev.net",
			ExtraHosts: []string{"app.orama.com"}}}
		// A value left in a copied env Secret is ignored: the operator's explicit entries win.
		o.Spec.Components.Dashboard.EnvFrom = []corev1.EnvFromSource{secretEnvFrom("v1-dashboard-env")}
		o.Spec.Cloudflare.APITokenSecretRef = &corev1.LocalObjectReference{Name: "cloudflare-api-token"} // origin TLS for the host
	})
	succeeded := func(uid string) *batchv1.Job {
		job := controlDBClusterRowJob(oc)
		job.UID = types.UID(uid)
		job.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(oc, oramav1alpha1.GroupVersion.WithKind("OramaCloudV1"))}
		job.Status.Succeeded = 1
		return job
	}
	stale := secretOf("v1-dashboard-env", map[string]string{dashboardAppURLEnv: "https://stale.example", dashboardPlacementClusterEnv: "stale-id"})
	cf := secretOf("cloudflare-api-token", map[string]string{"token": "cf-token", "zoneID": "zone", "accountID": "account"})
	dashEnv := func(t *testing.T, cl client.Client) map[string]string {
		t.Helper()
		dep := &appsv1.Deployment{}
		if err := cl.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: "v1-dashboard"}, dep); err != nil {
			t.Fatal(err)
		}
		env := map[string]string{}
		for _, e := range containerIn(t, dep.Spec.Template.Spec, "dashboard").Env {
			if e.ValueFrom == nil {
				env[e.Name] = e.Value
			}
		}
		return env
	}

	// The Job's pod reported the row id; a pod of another Job with the same name is not read.
	job := succeeded("job-uid")
	other := clusterRowPod(job, "older-run", corev1.PodSucceeded, "11111111-2222-4333-8444-555555555555")
	other.OwnerReferences[0].UID = "another-job"
	r, cl := clusterRowHarness(t, oc, testMasterKey, "Applied", job, stale, cf, other,
		clusterRowPod(job, "run", corev1.PodSucceeded, testClusterRowID+"\n"))
	if err := r.reconcileControlDBClusterRow(ctx, oc); err != nil {
		t.Fatal(err)
	}
	got, err := getClusterRowJob(t, cl, oc)
	if err != nil || got.Annotations[clusterRowIDKey] != testClusterRowID {
		t.Fatalf("recorded row id = %q (%v), want %s", got.Annotations[clusterRowIDKey], err, testClusterRowID)
	}
	if err := r.reconcileDashboard(ctx, oc); err != nil {
		t.Fatal(err)
	}
	env := dashEnv(t, cl)
	for name, want := range map[string]string{
		dashboardAppURLEnv:           "https://app-dev.oramadev.net",
		dashboardPlacementClusterEnv: testClusterRowID,
		"REDIS_CACHE_URL":            "http://v1-redis-http",
	} {
		if env[name] != want {
			t.Errorf("dashboard %s = %q, want %q", name, env[name], want)
		}
	}

	// A new dashboard host is a new origin.
	oc.Spec.Components.Dashboard.Host = "app.orama.ing"
	if err := r.reconcileDashboard(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if got := dashEnv(t, cl)[dashboardAppURLEnv]; got != "https://app.orama.ing" {
		t.Errorf("after a host change NEXT_PUBLIC_APP_URL = %q, want https://app.orama.ing", got)
	}
	oc.Spec.Components.Dashboard.Host = "app-dev.oramadev.net"

	// A recorded Job is never run again.
	if err := r.reconcileControlDBClusterRow(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if _, err := getClusterRowJob(t, cl, oc); err != nil {
		t.Fatalf("a recorded Job was replaced: %v", err)
	}

	// A succeeded Job from before #572 (no pod reported an id) is run again.
	r, cl = clusterRowHarness(t, oc, testMasterKey, "Applied", succeeded("old-uid"), stale, cf)
	if err := r.reconcileControlDBClusterRow(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if _, err := getClusterRowJob(t, cl, oc); !apierrors.IsNotFound(err) {
		t.Fatalf("a succeeded Job without a row id was kept: %v", err)
	}
	if err := r.reconcileDashboard(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if v, ok := dashEnv(t, cl)[dashboardPlacementClusterEnv]; ok {
		t.Errorf("dashboard placement %q before the row id is known", v)
	}
	if err := r.reconcileControlDBClusterRow(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if _, err := getClusterRowJob(t, cl, oc); err != nil {
		t.Fatalf("the Job was not created again: %v", err)
	}

	// A control DB without a live c1 row: recorded as none, and the dashboard gets no placement.
	job = succeeded("none-uid")
	r, cl = clusterRowHarness(t, oc, testMasterKey, "Applied", job, stale, cf, clusterRowPod(job, "run", corev1.PodSucceeded, clusterRowNone))
	if err := r.reconcileControlDBClusterRow(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if got, _ := getClusterRowJob(t, cl, oc); got.Annotations[clusterRowIDKey] != clusterRowNone {
		t.Fatalf("recorded %q, want %s", got.Annotations[clusterRowIDKey], clusterRowNone)
	}
	if err := r.reconcileDashboard(ctx, oc); err != nil {
		t.Fatal(err)
	}
	if v, ok := dashEnv(t, cl)[dashboardPlacementClusterEnv]; ok {
		t.Errorf("dashboard placement %q without a c1 row", v)
	}

	// Without a host the operator derives no origin.
	noHost := oc.DeepCopy()
	noHost.Spec.Components.Dashboard.Host = ""
	if err := r.reconcileDashboard(ctx, noHost); err != nil {
		t.Fatal(err)
	}
	if v, ok := dashEnv(t, cl)[dashboardAppURLEnv]; ok {
		t.Errorf("NEXT_PUBLIC_APP_URL = %q without a dashboard host", v)
	}
}
