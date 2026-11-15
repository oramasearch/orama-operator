/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// TestConnectionRolesIgnoreTheOwnersSearchPath proves operator#800 against a real server.
// The service's database owner is the tenant: it may set the database's default search_path and
// create objects in its own schemas. It puts a schema of its own ahead of pg_catalog and fills it
// with look-alikes of every catalog view, function and operator the operator uses, each of which
// records the caller and tries to make the owner a superuser. The operator then issues, rescopes,
// checks and revokes a connection role as the superuser. None of the look-alikes may run, and the
// owner must still not be a superuser.
//
//	CONNECTION_ROLES_TEST_POSTGRES=1 [PG_BIN=/opt/homebrew/opt/postgresql@17/bin] \
//	  go test ./internal/controller/ -run ConnectionRolesIgnoreTheOwnersSearchPath
func TestConnectionRolesIgnoreTheOwnersSearchPath(t *testing.T) {
	if os.Getenv("CONNECTION_ROLES_TEST_POSTGRES") != "1" {
		t.Skip("set CONNECTION_ROLES_TEST_POSTGRES=1 to run the connection roles against a throwaway Postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	admin := startThrowawayPostgres(t)

	superConn := connectAs(ctx, t, admin, admin.User, admin.Password)
	for _, statement := range []string{
		"CREATE ROLE app LOGIN PASSWORD 'owner-password'",
		"CREATE DATABASE app OWNER app",
	} {
		mustExec(ctx, t, superConn, statement)
	}
	_ = superConn.Close(ctx)
	admin.Database = "app"

	owner := connectAs(ctx, t, admin, "app", "owner-password")
	for _, statement := range []string{
		"CREATE SCHEMA analytics",
		"CREATE TABLE analytics.daily (day int PRIMARY KEY)",
		"CREATE SCHEMA evil",
		// Every call is recorded; the table is the owner's, so any caller may write it.
		"CREATE TABLE evil.calls (caller name NOT NULL, what text NOT NULL)",
		"GRANT INSERT ON evil.calls TO PUBLIC",
		// Run by a superuser, this makes the owner one. Run by anyone else it fails quietly.
		`CREATE FUNCTION evil.hook(what text) RETURNS boolean LANGUAGE plpgsql AS $f$
		 BEGIN
		   INSERT INTO evil.calls VALUES (current_user, what);
		   BEGIN
		     EXECUTE 'ALTER ROLE app SUPERUSER';
		   EXCEPTION WHEN insufficient_privilege THEN NULL;
		   END;
		   RETURN true;
		 END $f$`,
		"CREATE VIEW evil.pg_roles AS SELECT * FROM pg_catalog.pg_roles WHERE evil.hook('pg_roles')",
		"CREATE VIEW evil.pg_database AS SELECT * FROM pg_catalog.pg_database WHERE evil.hook('pg_database')",
		"CREATE VIEW evil.pg_namespace AS SELECT * FROM pg_catalog.pg_namespace WHERE evil.hook('pg_namespace')",
		"CREATE VIEW evil.pg_stat_activity AS SELECT * FROM pg_catalog.pg_stat_activity WHERE evil.hook('pg_stat_activity')",
		`CREATE FUNCTION evil.pg_get_userbyid(oid) RETURNS name LANGUAGE sql AS
		 $f$ SELECT pg_catalog.pg_get_userbyid($1) WHERE evil.hook('pg_get_userbyid') $f$`,
		`CREATE FUNCTION evil.current_database() RETURNS name LANGUAGE sql AS
		 $f$ SELECT pg_catalog.current_database() WHERE evil.hook('current_database') $f$`,
		`CREATE FUNCTION evil.pg_terminate_backend(integer) RETURNS boolean LANGUAGE sql AS
		 $f$ SELECT evil.hook('pg_terminate_backend') $f$`,
		`CREATE FUNCTION evil.name_eq(name, name) RETURNS boolean LANGUAGE sql AS
		 $f$ SELECT evil.hook('=') AND pg_catalog.nameeq($1, $2) $f$`,
		"CREATE OPERATOR evil.= (LEFTARG = name, RIGHTARG = name, FUNCTION = evil.name_eq)",
		`CREATE FUNCTION evil.name_text_eq(name, text) RETURNS boolean LANGUAGE sql AS
		 $f$ SELECT evil.hook('=') AND pg_catalog.nameeqtext($1, $2) $f$`,
		"CREATE OPERATOR evil.= (LEFTARG = name, RIGHTARG = text, FUNCTION = evil.name_text_eq)",
		// The owner may set its own database's default search_path: its schema comes first, ahead
		// of an explicitly listed pg_catalog.
		"ALTER DATABASE app SET search_path = evil, pg_catalog, public",
	} {
		mustExec(ctx, t, owner, statement)
	}
	_ = owner.Close(ctx)

	// The look-alikes work: a superuser session that trusts the database's search_path runs them.
	probe := connectAs(ctx, t, admin, admin.User, admin.Password)
	var path string
	if err := probe.QueryRow(ctx, "SHOW search_path").Scan(&path); err != nil || !strings.HasPrefix(path, "evil") {
		t.Fatalf("the owner's search_path is not in effect for a plain session: %q, %v", path, err)
	}
	_ = probe.Close(ctx)

	roles := pgxPostgresRoleManager{}
	role := connectionRoleName(testConnectionID)
	password, err := newConnectionPassword()
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := scramSHA256Verifier(password)
	if err != nil {
		t.Fatal(err)
	}
	if err := roles.SetRole(ctx, admin, role, verifier, oramav1alpha1.ConnectionCredentialScope{Access: oramav1alpha1.ConnectionAccessWrite}); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if err := roles.ApplyScope(ctx, admin, role, oramav1alpha1.ConnectionCredentialScope{Access: oramav1alpha1.ConnectionAccessRead, Schema: "analytics"}); err != nil {
		t.Fatalf("apply scope: %v", err)
	}
	if exists, err := roles.RoleExists(ctx, admin, role); err != nil || !exists {
		t.Fatalf("role exists = %v, %v", exists, err)
	}
	// An open session of the role, so revocation's pg_terminate_backend has a row to act on.
	open := connectAs(ctx, t, admin, role, password)
	if err := roles.DropRole(ctx, admin, role); err != nil {
		t.Fatalf("drop role: %v", err)
	}
	_ = open.Close(context.Background())

	check := connectAs(ctx, t, admin, admin.User, admin.Password)
	defer func() { _ = check.Close(context.Background()) }()
	rows, err := check.Query(ctx, "SELECT caller::text, what FROM evil.calls ORDER BY what")
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for rows.Next() {
		var caller, what string
		if err := rows.Scan(&caller, &what); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, caller+": "+what)
	}
	rows.Close()
	if len(calls) > 0 {
		t.Errorf("the operator's superuser session ran the owner's look-alikes: %v", calls)
	}
	var ownerIsSuperuser bool
	if err := check.QueryRow(ctx, "SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname OPERATOR(pg_catalog.=) 'app'").Scan(&ownerIsSuperuser); err != nil {
		t.Fatal(err)
	}
	if ownerIsSuperuser {
		t.Error("the database owner became a superuser")
	}
}

// TestConnectionPostgresSessionPinsSearchPath checks, without a server, that every session the
// operator opens as a service's superuser asks for search_path pg_catalog, pg_temp in the startup
// message, before any statement runs (operator#800).
func TestConnectionPostgresSessionPinsSearchPath(t *testing.T) {
	config, err := connectionPostgresConfig(ConnectionPostgresTarget{
		Host: "svc-rw.ns.svc", Port: 5432, Database: "app", User: "postgres", Password: "p@ss/word",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := config.RuntimeParams["search_path"]; got != trustedSearchPath {
		t.Fatalf("search_path = %q, want %q", got, trustedSearchPath)
	}
	if got := config.RuntimeParams["application_name"]; got != "orama-operator-connections" {
		t.Fatalf("application_name = %q", got)
	}
	if config.Database != "app" || config.User != "postgres" || config.Password != "p@ss/word" || config.Port != 5432 {
		t.Fatalf("config = %+v", config)
	}
}

// TestConnectionRoleSQLQualifiesEveryCatalogObject keeps every query the role manager runs written
// against pg_catalog explicitly, so it would still resolve correctly if the pinned search_path were
// ever lost (operator#800).
func TestConnectionRoleSQLQualifiesEveryCatalogObject(t *testing.T) {
	source, err := os.ReadFile("connection_postgres_roles.go")
	if err != nil {
		t.Fatal(err)
	}
	// Every pg_* or current_database name in a SQL string must follow "pg_catalog.".
	unqualified := regexp.MustCompile(`(^|[^.\w])(pg_(roles|database|namespace|stat_activity|get_userbyid|terminate_backend)|current_database)\b`)
	sqlString := regexp.MustCompile("`[^`]*`")
	for _, literal := range sqlString.FindAllString(string(source), -1) {
		if match := unqualified.FindString(literal); match != "" {
			t.Errorf("unqualified catalog reference %q in %s", strings.TrimSpace(match), literal)
		}
		if strings.Contains(literal, " = ") {
			t.Errorf("unqualified operator in %s; write OPERATOR(pg_catalog.=)", literal)
		}
	}
}
