// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestLakeSchemaEnvtest(t *testing.T) {
	if os.Getenv("LAKE_SCHEMA_TEST_POSTGRES") != "1" {
		t.Skip("set LAKE_SCHEMA_TEST_POSTGRES=1 for native Postgres schema proof")
	}
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Fatal("KUBEBUILDER_ASSETS must be set for the schema proof")
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{"../../config/crd/bases"},
		ErrorIfCRDPathMissing: true,
	}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	scheme := newLakeScheme(t)
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("replaces_completed_job", func(t *testing.T) {
		testLakeSchemaUpgrade(t, c, scheme)
	})
	for _, alreadyApplied := range []bool{false, true} {
		t.Run(fmt.Sprintf("already_applied_%t", alreadyApplied), func(t *testing.T) {
			proof := newLakeSchemaProof(t, c)
			initialSQL := strings.Split(lakeSchemaSQL, "-- Token attenuation (migration 0021)")[0]
			if alreadyApplied {
				initialSQL = lakeSchemaSQL
			}
			proof.sql(initialSQL)
			proof.seedSessions()
			output := proof.applyLakeSchema(scheme)
			skipped := strings.Contains(output, "schema already applied ("+lakeSchemaGuardTable+" present, every schema_migrations row recorded)")
			if alreadyApplied != skipped || alreadyApplied == strings.Contains(output, "applying schema") {
				t.Fatalf("unexpected guard behavior: %s", output)
			}
			proof.verifyAttenuation(alreadyApplied)
			proof.verifySteadyStatus()
		})
	}
	// #271: 0067 merged after 0068 and 0069 had applied. The database holds the newest guard but
	// lacks 0067's column, completion table and schema_migrations row; the Job must apply the file.
	t.Run("guard_present_earlier_migration_missing", func(t *testing.T) {
		proof := newLakeSchemaProof(t, c)
		proof.sql(lakeSchemaSQL)
		proof.seedSessions()
		proof.sql(`ALTER TABLE workspaces DROP COLUMN store_moved_at;
DROP TABLE workspace_store_guard;
DELETE FROM schema_migrations WHERE version = '0067_workspace_store';`)
		output := proof.applyLakeSchema(scheme)
		if !strings.Contains(output, lakeSchemaGuardTable+" present but schema_migrations lacks: 0067_workspace_store") ||
			!strings.Contains(output, "applying schema") || strings.Contains(output, "schema already applied") {
			t.Fatalf("missing migration was not re-applied: %s", output)
		}
		if got := proof.sql("SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'workspaces' AND column_name = 'store_moved_at'"); got != "1" {
			t.Fatalf("workspaces.store_moved_at count = %s", got)
		}
		if got := proof.sql("SELECT to_regclass('public.workspace_store_guard') IS NOT NULL"); got != "t" {
			t.Fatal("workspace_store_guard missing after re-apply")
		}
		if got := proof.sql("SELECT count(*) FROM schema_migrations WHERE version = '0067_workspace_store'"); got != "1" {
			t.Fatalf("0067 schema_migrations rows = %s", got)
		}
		proof.verifyAttenuation(true)
		proof.verifySteadyStatus()
		t.Log("0067 re-applied under an existing 0069 guard; earlier one-time migrations did not re-run")
	})
}

// applyLakeSchema creates the Lake's schema resources, runs the rendered migration Job against the
// proof database and returns its output, checking the phase before and after.
func (p *lakeSchemaProof) applyLakeSchema(scheme *runtime.Scheme) string {
	p.t.Helper()
	lakeReconciler := &LakeReconciler{Client: p.client, Scheme: scheme}
	if err := lakeReconciler.reconcileLakeControlDBSchema(p.ctx, p.lake, p.lake.Spec.Namespace, nil, nil); err != nil {
		p.t.Fatal(err)
	}
	p.reconciler = &PostgresSchemaReconciler{Client: p.client, Scheme: scheme}
	p.reconcile("Applying")
	output := p.runJob()
	p.reconcile("Applied")
	return output
}

func testLakeSchemaUpgrade(t *testing.T, c client.Client, scheme *runtime.Scheme) {
	proof := newLakeSchemaProof(t, c)
	proof.sql(strings.Split(lakeSchemaSQL, "-- Token attenuation (migration 0021)")[0])
	proof.seedSessions()
	lakeReconciler := &LakeReconciler{Client: c, Scheme: scheme}
	if err := lakeReconciler.reconcileLakeControlDBSchema(proof.ctx, proof.lake, proof.key.Namespace, nil, nil); err != nil {
		t.Fatal(err)
	}
	var ps oramav1alpha1.PostgresSchema
	if err := c.Get(proof.ctx, proof.key, &ps); err != nil {
		t.Fatal(err)
	}
	ps.Spec.Version = "sha256:9ed417711b2f"
	ps.Spec.GuardTable = "identity_exit_guard"
	if err := c.Update(proof.ctx, &ps); err != nil {
		t.Fatal(err)
	}
	proof.reconciler = &PostgresSchemaReconciler{Client: c, Scheme: scheme}
	proof.reconcile("Applying")
	proof.runJob()
	proof.reconcile("Applied")
	if err := lakeReconciler.reconcileLakeControlDBSchema(proof.ctx, proof.lake, proof.key.Namespace, nil, nil); err != nil {
		t.Fatal(err)
	}
	proof.reconcile("Applying")
	proof.finishJobDeletion()
	proof.reconcile("Applying")
	proof.runJob()
	proof.reconcile("Applied")
	proof.verifyAttenuation(false)
	proof.verifySteadyStatus()
}

// envtest does not run Kubernetes garbage collection. Confirm the controller requested foreground
// deletion, then finish that deletion after the native Job process has exited (there are no pods).
func (p *lakeSchemaProof) finishJobDeletion() {
	p.t.Helper()
	var job batchv1.Job
	key := client.ObjectKey{Namespace: p.key.Namespace, Name: p.key.Name + "-migrate"}
	if err := p.client.Get(p.ctx, key, &job); err != nil {
		p.t.Fatal(err)
	}
	if job.DeletionTimestamp.IsZero() {
		p.t.Fatal("obsolete Job was not deleted")
	}
	found := false
	for _, finalizer := range job.Finalizers {
		if finalizer == metav1.FinalizerDeleteDependents {
			found = true
		}
	}
	if !found {
		p.t.Fatal("obsolete Job deletion must wait for its pods")
	}
	p.reconcile("Applying")
	job.Finalizers = nil
	if err := p.client.Update(p.ctx, &job); err != nil {
		p.t.Fatal(err)
	}
	p.t.Log("obsolete completed Job removed with foreground deletion before replacement")
}

func (p *lakeSchemaProof) verifySteadyStatus() {
	p.t.Helper()
	var before oramav1alpha1.PostgresSchema
	if err := p.client.Get(p.ctx, p.key, &before); err != nil {
		p.t.Fatal(err)
	}
	p.reconcile("Applied")
	var after oramav1alpha1.PostgresSchema
	if err := p.client.Get(p.ctx, p.key, &after); err != nil {
		p.t.Fatal(err)
	}
	if before.ResourceVersion != after.ResourceVersion {
		p.t.Fatal("steady Applied schema rewrote status")
	}
	p.t.Log("steady reconcile: no status write")
}

type lakeSchemaProof struct {
	t          *testing.T
	ctx        context.Context
	client     client.Client
	lake       *oramav1alpha1.Lake
	key        client.ObjectKey
	uri        string
	reconciler *PostgresSchemaReconciler
}

func newLakeSchemaProof(t *testing.T, c client.Client) *lakeSchemaProof {
	t.Helper()
	name := fmt.Sprintf("issue160_%d", time.Now().UnixNano())
	user := os.Getenv("PGUSER")
	if user == "" {
		user = os.Getenv("USER")
	}
	runSchemaCommand(t, "createdb", "-h", "127.0.0.1", "-U", user, name)
	t.Cleanup(func() {
		runSchemaCommand(t, "dropdb", "-h", "127.0.0.1", "-U", user, name)
	})
	ns := strings.ReplaceAll(name, "_", "-")
	proof := &lakeSchemaProof{
		t:      t,
		ctx:    context.Background(),
		client: c,
		lake: &oramav1alpha1.Lake{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
			Spec:       oramav1alpha1.LakeSpec{Namespace: ns, Image: "oramasearch/lake:issue160"},
		},
		key: client.ObjectKey{Namespace: ns, Name: lakeControlDB(ns) + "-schema"},
		uri: "postgresql://" + user + "@127.0.0.1:5432/" + name,
	}
	for _, obj := range []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		proof.lake,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: lakeControlDB(ns) + "-app", Namespace: ns},
			Data:       map[string][]byte{"uri": []byte(proof.uri)},
		},
	} {
		if err := c.Create(proof.ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	return proof
}

func runSchemaCommand(t *testing.T, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", name, err, output)
	}
	return strings.TrimSpace(string(output))
}

func (p *lakeSchemaProof) sql(query string) string {
	p.t.Helper()
	return runSchemaCommand(p.t, "psql", p.uri, "-X", "-qAt", "-v", "ON_ERROR_STOP=1", "-c", query)
}

func (p *lakeSchemaProof) seedSessions() {
	p.t.Helper()
	p.sql(`INSERT INTO users (id) VALUES ('00000000-0000-0000-0000-000000000001');
INSERT INTO sessions (user_id, kind, secret_hash, reason, expires_at)
SELECT '00000000-0000-0000-0000-000000000001', kind, kind, 'schema proof', now() + interval '1 day'
FROM unnest(ARRAY['api_token', 'static', 'browser']) AS kind;`)
}

func (p *lakeSchemaProof) reconcile(want string) {
	p.t.Helper()
	if _, err := p.reconciler.Reconcile(p.ctx, ctrl.Request{NamespacedName: p.key}); err != nil {
		p.t.Fatal(err)
	}
	var ps oramav1alpha1.PostgresSchema
	if err := p.client.Get(p.ctx, p.key, &ps); err != nil {
		p.t.Fatal(err)
	}
	if ps.Status.Phase != want {
		p.t.Fatalf("schema phase = %s, want %s", ps.Status.Phase, want)
	}
	if want == "Applied" && ps.Status.AppliedVersion != ps.Spec.Version {
		p.t.Fatalf("applied %s, want %s", ps.Status.AppliedVersion, ps.Spec.Version)
	}
	p.t.Logf("reconcile: %s, applied=%s, desired=%s", ps.Status.Phase, ps.Status.AppliedVersion, ps.Spec.Version)
}

// envtest has no kubelet or Job controller. Execute the rendered command natively, changing
// only its ConfigMap mount path, then report the successful process through Job.status.
func (p *lakeSchemaProof) runJob() string {
	p.t.Helper()
	var job batchv1.Job
	if err := p.client.Get(p.ctx, client.ObjectKey{Namespace: p.key.Namespace, Name: p.key.Name + "-migrate"}, &job); err != nil {
		p.t.Fatal(err)
	}
	var cm corev1.ConfigMap
	if err := p.client.Get(p.ctx, client.ObjectKey{Namespace: p.key.Namespace, Name: lakeControlDB(p.lake.Name) + "-migrations"}, &cm); err != nil {
		p.t.Fatal(err)
	}
	path := filepath.Join(p.t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(cm.Data["schema.sql"]), 0600); err != nil {
		p.t.Fatal(err)
	}
	command := job.Spec.Template.Spec.Containers[0].Command
	script := strings.ReplaceAll(command[2], "/migrations/schema.sql", "'"+strings.ReplaceAll(path, "'", "'\"'\"'")+"'")
	p.t.Setenv("DATABASE_URL", p.uri)
	output := runSchemaCommand(p.t, command[0], command[1], script)
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "schema already") || strings.HasPrefix(line, "applying schema") || strings.HasPrefix(line, "postgres-schema:") || strings.Contains(line, "schema_migrations lacks") {
			p.t.Log(line)
		}
	}
	job.Status.Succeeded = 1
	if err := p.client.Status().Update(p.ctx, &job); err != nil {
		p.t.Fatal(err)
	}
	return output
}

func (p *lakeSchemaProof) verifyAttenuation(alreadyApplied bool) {
	p.t.Helper()
	if got := p.sql("SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'sessions' AND column_name = 'parent_session_id'"); got != "1" {
		p.t.Fatalf("PostgresSchema reported Applied with parent_session_id missing (column count %s)", got)
	}
	if got := p.sql("SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'sessions_parent_session_id_idx'"); got != "1" {
		p.t.Fatalf("parent session index count = %s", got)
	}
	if got := p.sql("SELECT count(*) FROM pg_constraint WHERE conrelid = 'sessions'::regclass AND contype = 'f' AND conname = 'sessions_parent_session_id_fkey' AND confdeltype = 'c'"); got != "1" {
		p.t.Fatalf("parent session cascading foreign key count = %s", got)
	}
	versions := schemaMigrationVersions(lakeSchemaSQL)
	newest := versions[0]
	for _, version := range versions {
		if version > newest {
			newest = version
		}
	}
	if got := p.sql("SELECT max(version) FROM schema_migrations"); got != newest {
		p.t.Fatalf("newest migration = %s, want %s", got, newest)
	}
	if got := p.sql("SELECT count(*) FROM schema_migrations"); got != fmt.Sprint(len(versions)) {
		p.t.Fatalf("schema_migrations rows = %s, want %d", got, len(versions))
	}
	wantRevoked := "2"
	if alreadyApplied {
		wantRevoked = "0"
	}
	if got := p.sql("SELECT count(*) FROM sessions WHERE kind IN ('api_token', 'static') AND revoked_at IS NOT NULL"); got != wantRevoked {
		p.t.Fatalf("revoked sessions = %s, want %s", got, wantRevoked)
	}
	// 0022_session_lookup revokes every pre-migration session once, browser sessions included:
	// a row without a lookup UUID can no longer authenticate. The seeded sessions have none, so a
	// first application revokes the browser session too; a database already at the newest
	// migration leaves new sessions alone.
	wantBrowserRevoked := "1"
	if alreadyApplied {
		wantBrowserRevoked = "0"
	}
	if got := p.sql("SELECT count(*) FROM sessions WHERE kind = 'browser' AND revoked_at IS NOT NULL"); got != wantBrowserRevoked {
		p.t.Fatalf("revoked browser sessions = %s, want %s", got, wantBrowserRevoked)
	}
	p.t.Logf("0021 verified: parent column, cascading FK, index, migration marker; revoked=%s", wantRevoked)
}
