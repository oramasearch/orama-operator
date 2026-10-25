/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func startMigrateEnvtest(t *testing.T) client.Client {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for the PostgresSchema migrate envtest")
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
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The API server accepts a PostgresSchema with a schema file or with spec.migrate, and refuses
// one with neither; a Job with the rendered pod failure policy is accepted too.
func TestPostgresSchemaMigrateValidationEnvtest(t *testing.T) {
	c := startMigrateEnvtest(t)
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "migrate"}}); err != nil {
		t.Fatal(err)
	}
	schema := func(name string, spec oramav1alpha1.PostgresSchemaSpec) *oramav1alpha1.PostgresSchema {
		spec.ConnectionSecretRef = oramav1alpha1.PostgresSecretKeyRef{Name: "db", Key: "uri"}
		spec.Version = "v1"
		return &oramav1alpha1.PostgresSchema{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "migrate"}, Spec: spec}
	}
	migrate := schema("from-image", oramav1alpha1.PostgresSchemaSpec{
		Image: testLakeManifestImage,
		Migrate: &oramav1alpha1.PostgresSchemaMigrate{
			Args: []string{"migrate", "--wait-seconds", "600"}, DatabaseEnv: "LAKE_CONTROL_DB_URI", FailOnExitCodes: []int32{1, 2},
			RunAsUser: ptrInt64(lakeRunAsUser), RunAsGroup: ptrInt64(lakeRunAsGroup),
		},
	})
	if err := c.Create(ctx, migrate); err != nil {
		t.Fatalf("migrate-only PostgresSchema refused: %v", err)
	}
	if err := c.Create(ctx, schema("from-file", oramav1alpha1.PostgresSchemaSpec{
		SchemaConfigMapRef: oramav1alpha1.PostgresConfigMapKeyRef{Name: "schema", Key: "schema.sql"},
	})); err != nil {
		t.Fatalf("file PostgresSchema refused: %v", err)
	}
	err := c.Create(ctx, schema("neither", oramav1alpha1.PostgresSchemaSpec{}))
	if err == nil || !strings.Contains(err.Error(), "set schemaConfigMapRef, or migrate") {
		t.Fatalf("PostgresSchema with neither accepted: %v", err)
	}
	bad := schema("bad-env", oramav1alpha1.PostgresSchemaSpec{Migrate: &oramav1alpha1.PostgresSchemaMigrate{Args: []string{"migrate"}, DatabaseEnv: "lake uri"}})
	if err := c.Create(ctx, bad); err == nil {
		t.Fatal("an invalid databaseEnv was accepted")
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "from-image-migrate", Namespace: "migrate"}, Spec: pgMigrationJobSpec(migrate)}
	if err := c.Create(ctx, job); err != nil {
		t.Fatalf("rendered migrate Job refused: %v", err)
	}
	t.Log("API server: migrate-only and file-only accepted, neither refused, rendered Job accepted")
}

// lakeMigrateReport is `lake migrate`'s JSON line.
type lakeMigrateReport struct {
	OK                bool     `json:"ok"`
	Outcome           string   `json:"outcome"`
	MigrationsApplied []string `json:"migrations_applied"`
}

// TestLakeImageMigrateEnvtest runs the migration Job the operator renders for a supported image
// with a real lake binary (from lake#1147 onward) against a scratch Postgres database: the Lake's
// PostgresSchema in migrate mode, the PostgresSchema controller's Job, its arguments and
// environment, executed natively; then Applied. A second run finds the database current; a
// deleted migration row is applied again.
//
//	LAKE_SCHEMA_TEST_POSTGRES=1 LAKE_MIGRATE_TEST_BINARY=<lake build>/lake go test ./internal/controller -run TestLakeImageMigrateEnvtest
func TestLakeImageMigrateEnvtest(t *testing.T) {
	binary := os.Getenv("LAKE_MIGRATE_TEST_BINARY")
	if os.Getenv("LAKE_SCHEMA_TEST_POSTGRES") != "1" || binary == "" {
		t.Skip("set LAKE_SCHEMA_TEST_POSTGRES=1 and LAKE_MIGRATE_TEST_BINARY=<lake binary with `lake migrate`>")
	}
	c := startMigrateEnvtest(t)
	scheme := newLakeScheme(t)
	proof := newLakeSchemaProof(t, c)

	// The manifest comes from the binary under test, as the manifest Job would read it.
	output, err := exec.Command(binary, "manifest").Output()
	if err != nil {
		t.Fatalf("lake manifest: %v", err)
	}
	manifest, err := parseLakeManifest(string(output), 0)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Image = proof.lake.Spec.Image
	manifest.ImageRef = testLakeManifestImage
	lakeReconciler := &LakeReconciler{Client: c, Scheme: scheme}
	if err := lakeReconciler.reconcileLakeControlDBSchema(proof.ctx, proof.lake, proof.key.Namespace, nil, manifest); err != nil {
		t.Fatal(err)
	}
	proof.reconciler = &PostgresSchemaReconciler{Client: c, Scheme: scheme}
	proof.reconcile("Applying")
	report, code := runLakeMigrateJob(t, proof, binary)
	if code != 0 || !report.OK || report.Outcome != "applied" || len(report.MigrationsApplied) != len(manifest.Migrations) {
		t.Fatalf("first run: exit %d, %+v", code, report)
	}
	markJobSucceeded(t, proof)
	proof.reconcile("Applied")
	if got := proof.sql("SELECT count(*) FROM schema_migrations"); got != strconv.Itoa(len(manifest.Migrations)) {
		t.Fatalf("schema_migrations rows = %s, want %d", got, len(manifest.Migrations))
	}
	if got := proof.sql("SELECT to_regclass('public." + manifest.GuardTable + "') IS NOT NULL"); got != "t" {
		t.Fatal("newest guard missing after lake migrate")
	}
	t.Logf("first run: applied %d migrations, PostgresSchema Applied at %s", len(report.MigrationsApplied), lakeImageSchemaVersion(proof.lake, manifest))

	report, code = runLakeMigrateJob(t, proof, binary)
	if code != 0 || report.Outcome != "current" || len(report.MigrationsApplied) != 0 {
		t.Fatalf("second run: exit %d, %+v", code, report)
	}
	t.Log("second run: current, nothing applied")

	last := manifest.Migrations[len(manifest.Migrations)-1]
	proof.sql("DELETE FROM schema_migrations WHERE version = '" + last + "'")
	report, code = runLakeMigrateJob(t, proof, binary)
	if code != 0 || report.Outcome != "applied" || len(report.MigrationsApplied) != 1 || report.MigrationsApplied[0] != last {
		t.Fatalf("missing row: exit %d, %+v", code, report)
	}
	t.Logf("missing %s: applied again alone", last)
}

// runLakeMigrateJob executes the rendered migrate Job's container natively: the binary stands in
// for the image's entrypoint, with the Job's arguments and its one environment variable (from the
// connection Secret). Returns the JSON report and the exit status.
func runLakeMigrateJob(t *testing.T, proof *lakeSchemaProof, binary string) (lakeMigrateReport, int) {
	t.Helper()
	var job batchv1.Job
	if err := proof.client.Get(proof.ctx, client.ObjectKey{Namespace: proof.key.Namespace, Name: proof.key.Name + "-migrate"}, &job); err != nil {
		t.Fatal(err)
	}
	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != testLakeManifestImage || container.Command != nil || len(container.Env) != 1 {
		t.Fatalf("migrate container = %+v", container)
	}
	var secret corev1.Secret
	ref := container.Env[0].ValueFrom.SecretKeyRef
	if err := proof.client.Get(proof.ctx, client.ObjectKey{Namespace: proof.key.Namespace, Name: ref.Name}, &secret); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, container.Args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), container.Env[0].Name + "=" + string(secret.Data[ref.Key])}
	stdout, err := cmd.Output()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	var report lakeMigrateReport
	line := strings.TrimSpace(string(stdout))
	if i := strings.LastIndex(line, "\n"); i >= 0 {
		line = line[i+1:]
	}
	if err := json.Unmarshal([]byte(line), &report); err != nil {
		t.Fatalf("lake migrate printed no JSON report (exit %d): %s", code, stdout)
	}
	return report, code
}

func markJobSucceeded(t *testing.T, proof *lakeSchemaProof) {
	t.Helper()
	var job batchv1.Job
	if err := proof.client.Get(proof.ctx, client.ObjectKey{Namespace: proof.key.Namespace, Name: proof.key.Name + "-migrate"}, &job); err != nil {
		t.Fatal(err)
	}
	job.Status.Succeeded = 1
	if err := proof.client.Status().Update(proof.ctx, &job); err != nil {
		t.Fatal(err)
	}
}
