// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestRehearsalReadOnlyRoleAgainstPostgres(t *testing.T) {
	uri := os.Getenv("REHEARSAL_TEST_POSTGRES_URI")
	if uri == "" {
		t.Skip("REHEARSAL_TEST_POSTGRES_URI is required for local database privilege proof")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	database, login := "rehearsal_"+suffix, "rehearsal_reader_"+suffix
	exec := func(sql string) {
		t.Helper()
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	exec("CREATE ROLE " + login + " LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS INHERIT CONNECTION LIMIT 4 PASSWORD 'local-test-password'")
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
		if _, err := admin.Exec(cleanup, "DROP ROLE "+login); err != nil {
			t.Error(err)
		}
	})
	// Apply precisely the membership that the CNPG role declaration requests.
	role := lakeRehearsalRole("unused-fixture")
	for _, member := range role["inRoles"].([]interface{}) {
		exec("GRANT " + member.(string) + " TO " + login)
	}
	exec("CREATE DATABASE " + database)
	config, err := pgx.ParseConfig(uri)
	if err != nil {
		t.Fatal(err)
	}
	config.Database = database
	owner, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	// Match PostgreSQL 15+ public-schema defaults when the disposable server is older.
	if _, err := owner.Exec(ctx, "REVOKE CREATE ON SCHEMA public FROM PUBLIC"); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, "CREATE TABLE verification_guard (id int); INSERT INTO verification_guard VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	config.User, config.Password = login, "local-test-password"
	reader, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close(context.Background())
	var count int
	if err := reader.QueryRow(ctx, "SELECT count(*) FROM verification_guard").Scan(&count); err != nil || count != 1 {
		t.Fatalf("verification read failed: count=%d err=%v", count, err)
	}
	for _, sql := range []string{"INSERT INTO verification_guard VALUES (2)", "UPDATE verification_guard SET id=2", "DELETE FROM verification_guard", "TRUNCATE verification_guard", "DROP TABLE verification_guard", "CREATE TABLE public.unexpected_write (id int)", "ALTER ROLE " + login + " SUPERUSER", "SET ROLE " + pgx.Identifier{admin.Config().User}.Sanitize()} {
		if _, err := reader.Exec(ctx, sql); err == nil {
			t.Errorf("read-only login accepted %s", strings.Fields(sql)[0])
		}
	}
	// Future schema tables are covered without granting write privileges or rerunning grants SQL.
	if _, err := owner.Exec(ctx, "CREATE TABLE future_guard (id int); INSERT INTO future_guard VALUES (3)"); err != nil {
		t.Fatal(err)
	}
	if err := reader.QueryRow(ctx, "SELECT count(*) FROM future_guard").Scan(&count); err != nil || count != 1 {
		t.Fatalf("future table read failed: %v", err)
	}
}
