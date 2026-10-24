/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// TestSchemaMigrationSedMatchesGo runs the Job script's sed extraction over the vendored lake
// schema and checks it finds exactly the versions schemaMigrationVersions finds, so the Job and the
// restore rehearsal agree on which migrations the schema records.
func TestSchemaMigrationSedMatchesGo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(lakeSchemaSQL), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("sh", "-c", pgSchemaMigrationSed+" "+path).CombinedOutput()
	if err != nil {
		t.Fatalf("sed: %v\n%s", err, output)
	}
	fromSed := strings.Fields(string(output))
	fromGo := schemaMigrationVersions(lakeSchemaSQL)
	if !reflect.DeepEqual(fromSed, fromGo) {
		t.Fatalf("sed found %d versions, Go found %d:\nsed: %v\ngo:  %v", len(fromSed), len(fromGo), fromSed, fromGo)
	}
	if want := strings.Count(lakeSchemaSQL, "INSERT INTO schema_migrations (version) VALUES ('"); len(fromGo) != want {
		t.Fatalf("extracted %d versions, schema has %d migration records", len(fromGo), want)
	}
	found := false
	for _, version := range fromGo {
		if version == "0067_workspace_store" {
			found = true
		}
	}
	if !found {
		t.Fatal("0067_workspace_store not extracted")
	}
}

// fakePsql answers the Job script's queries from environment variables: FAKE_GUARD=1 makes the
// guard table exist, FAKE_MIGRATIONS_TABLE=1 makes schema_migrations exist, and FAKE_APPLIED lists
// the versions it holds. The missing-migration query is answered from the VALUES list the script
// actually sent, so the test checks the script's extraction and comparison, not a canned answer.
const fakePsql = `#!/bin/sh
args="$*"
case "$args" in
  *string_agg*)
    missing=""
    for v in $(printf '%s\n' "$args" | grep -o "('[0-9][0-9][0-9][0-9]_[A-Za-z0-9_]*')" | tr -d "()'"); do
      case " $FAKE_APPLIED " in
        *" $v "*) ;;
        *) missing="$missing $v" ;;
      esac
    done
    echo "${missing# }"
    ;;
  *--single-transaction*)
    echo "FAKE: applied schema file"
    ;;
  *"to_regclass('public.schema_migrations')"*)
    if [ "${FAKE_MIGRATIONS_TABLE:-}" = 1 ]; then echo schema_migrations; fi
    ;;
  *to_regclass*)
    if [ "${FAKE_GUARD:-}" = 1 ]; then echo workspace_volumes_guard; fi
    ;;
esac
exit 0
`

func runPgMigrationScript(t *testing.T, schema string, env ...string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "psql"), []byte(fakePsql), 0o700); err != nil {
		t.Fatal(err)
	}
	schemaPath := filepath.Join(dir, "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(schema), 0o600); err != nil {
		t.Fatal(err)
	}
	ps := pgSchemaCR()
	ps.Spec.GuardTable = "workspace_volumes_guard"
	command := pgMigrationJobSpec(ps).Template.Spec.Containers[0].Command
	script := strings.ReplaceAll(command[2], "/migrations/schema.sql", schemaPath)
	cmd := exec.Command(command[0], command[1], script)
	cmd.Env = append([]string{"PATH=" + dir + ":" + os.Getenv("PATH"), "DATABASE_URL=postgresql://fake"}, env...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script: %v\n%s", err, output)
	}
	return string(output)
}

// TestPgMigrationScriptDecidesFromEveryMigration covers the #271 regression: a database holding
// the newest guard but missing an earlier migration's schema_migrations row re-applies the schema.
func TestPgMigrationScriptDecidesFromEveryMigration(t *testing.T) {
	all := schemaMigrationVersions(lakeSchemaSQL)
	var withoutStore []string
	for _, version := range all {
		if version != "0067_workspace_store" {
			withoutStore = append(withoutStore, version)
		}
	}
	cases := []struct {
		name    string
		schema  string
		env     []string
		applies bool
		message string
	}{
		{name: "fresh database", schema: lakeSchemaSQL, applies: true},
		{name: "guard present, earlier migration missing", schema: lakeSchemaSQL, applies: true,
			env:     []string{"FAKE_GUARD=1", "FAKE_MIGRATIONS_TABLE=1", "FAKE_APPLIED=" + strings.Join(withoutStore, " ")},
			message: "workspace_volumes_guard present but schema_migrations lacks: 0067_workspace_store"},
		{name: "guard present, every migration recorded", schema: lakeSchemaSQL,
			env:     []string{"FAKE_GUARD=1", "FAKE_MIGRATIONS_TABLE=1", "FAKE_APPLIED=" + strings.Join(all, " ")},
			message: "schema already applied (workspace_volumes_guard present, every schema_migrations row recorded) — nothing to do"},
		{name: "guard present, no schema_migrations table", schema: lakeSchemaSQL,
			env:     []string{"FAKE_GUARD=1"},
			message: "schema already applied (workspace_volumes_guard present, no schema_migrations table) — nothing to do"},
		{name: "schema records no migrations", schema: "CREATE TABLE IF NOT EXISTS crawler_configs (id int);\n",
			env:     []string{"FAKE_GUARD=1"},
			message: "schema already applied (workspace_volumes_guard present) — nothing to do"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output := runPgMigrationScript(t, tc.schema, tc.env...)
			if applied := strings.Contains(output, "FAKE: applied schema file"); applied != tc.applies {
				t.Fatalf("applied = %t, want %t\n%s", applied, tc.applies, output)
			}
			if tc.message != "" && !strings.Contains(output, tc.message) {
				t.Fatalf("output lacks %q\n%s", tc.message, output)
			}
		})
	}
}

// TestRehearsalQueriesNameMissingMigrations checks the restore rehearsal asks for every migration
// the vendored schema records, and that both verification scripts fail on a non-empty answer.
func TestRehearsalQueriesNameMissingMigrations(t *testing.T) {
	for _, version := range schemaMigrationVersions(lakeSchemaSQL) {
		if !strings.Contains(postgresMissingMigrationsQuery, "('"+version+"')") {
			t.Errorf("missing-migration query lacks %s", version)
		}
	}
	if !strings.Contains(postgresVerificationQuery, "'missing_migrations=' || ("+postgresMissingMigrationsQuery+")") {
		t.Error("restore comparison query lacks the missing_migrations row")
	}
	if !strings.Contains(postgresRestoreVerificationScript, "grep -qx 'missing_migrations=' /tmp/$side") {
		t.Error("restore verification does not fail on missing migrations")
	}
	if !strings.Contains(postgresConnectionVerificationScript, `"$MIGRATIONS_QUERY"`) {
		t.Error("failover verification does not check missing migrations")
	}
	job := verificationJobSpec(&oramav1alpha1.Lake{}, "source", "", "secret", false)
	found := false
	for _, env := range job.Template.Spec.Containers[0].Env {
		if env.Name == "MIGRATIONS_QUERY" && env.Value == postgresMissingMigrationsQuery {
			found = true
		}
	}
	if !found {
		t.Error("verification Job lacks MIGRATIONS_QUERY")
	}
	if got := missingMigrationsQuery(nil); got != "SELECT ''" {
		t.Errorf("empty query = %q", got)
	}
}
