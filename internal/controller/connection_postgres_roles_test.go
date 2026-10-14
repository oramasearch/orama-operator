/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// TestConnectionRolesAgainstPostgres proves operator#469's acceptance against a real server: a
// read-only connection can read and cannot write, a schema scope stops at its schema, rotation
// refuses the old password, and revocation ends an open session and every new sign-in at once. It
// starts its own throwaway cluster with initdb and SCRAM authentication on a free local port, so
// passwords are really checked and no shared server is touched; the cluster is removed afterwards.
//
//	CONNECTION_ROLES_TEST_POSTGRES=1 [PG_BIN=/opt/homebrew/opt/postgresql@17/bin] \
//	  go test ./internal/controller/ -run ConnectionRolesAgainstPostgres
func TestConnectionRolesAgainstPostgres(t *testing.T) {
	if os.Getenv("CONNECTION_ROLES_TEST_POSTGRES") != "1" {
		t.Skip("set CONNECTION_ROLES_TEST_POSTGRES=1 to run the connection roles against a throwaway Postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	admin := startThrowawayPostgres(t)

	// The service as Lake's CloudNativePG cluster bootstraps it: database app owned by role app,
	// whose tables the app created in public and in a second schema.
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
	defer func() { _ = owner.Close(context.Background()) }()
	for _, statement := range []string{
		"CREATE TABLE public.orders (id serial PRIMARY KEY, total int NOT NULL)",
		"INSERT INTO public.orders (total) VALUES (10)",
		"CREATE SCHEMA analytics",
		"CREATE TABLE analytics.daily (day int PRIMARY KEY, total int NOT NULL)",
		"INSERT INTO analytics.daily VALUES (1, 10)",
	} {
		mustExec(ctx, t, owner, statement)
	}

	roles := pgxPostgresRoleManager{}
	role := connectionRoleName(testConnectionID)
	issue := func(scope oramav1alpha1.ConnectionCredentialScope) string {
		t.Helper()
		password, err := newConnectionPassword()
		if err != nil {
			t.Fatal(err)
		}
		verifier, err := scramSHA256Verifier(password)
		if err != nil {
			t.Fatal(err)
		}
		if err := roles.SetRole(ctx, admin, role, verifier, scope); err != nil {
			t.Fatalf("set role %+v: %v", scope, err)
		}
		return password
	}

	// Read: reads every schema, writes nothing, creates nothing.
	password := issue(oramav1alpha1.ConnectionCredentialScope{Access: oramav1alpha1.ConnectionAccessRead})
	if exists, err := roles.RoleExists(ctx, admin, role); err != nil || !exists {
		t.Fatalf("role exists = %v, %v", exists, err)
	}
	reader := connectAs(ctx, t, admin, role, password)
	expectRows(ctx, t, reader, "SELECT count(*) FROM public.orders", 1)
	expectRows(ctx, t, reader, "SELECT count(*) FROM analytics.daily", 1)
	expectDenied(ctx, t, reader, "INSERT INTO public.orders (total) VALUES (20)")
	expectDenied(ctx, t, reader, "UPDATE analytics.daily SET total = 0")
	expectDenied(ctx, t, reader, "CREATE TABLE public.intruder (id int)")
	_ = reader.Close(ctx)

	// Write: reads and writes rows, still no DDL.
	password = issue(oramav1alpha1.ConnectionCredentialScope{Access: oramav1alpha1.ConnectionAccessWrite})
	writer := connectAs(ctx, t, admin, role, password)
	mustExec(ctx, t, writer, "INSERT INTO public.orders (total) VALUES (20)")
	mustExec(ctx, t, writer, "DELETE FROM analytics.daily WHERE day = 99")
	expectDenied(ctx, t, writer, "CREATE TABLE public.intruder (id int)")
	expectDenied(ctx, t, writer, "DROP TABLE public.orders")
	_ = writer.Close(ctx)

	// Read on one schema: that schema only, including a table the owner creates afterwards; the
	// previous write grants are gone.
	password = issue(oramav1alpha1.ConnectionCredentialScope{Access: oramav1alpha1.ConnectionAccessRead, Schema: "analytics"})
	mustExec(ctx, t, owner, "CREATE TABLE analytics.weekly (week int)")
	scoped := connectAs(ctx, t, admin, role, password)
	expectRows(ctx, t, scoped, "SELECT count(*) FROM analytics.daily", 1)
	expectRows(ctx, t, scoped, "SELECT count(*) FROM analytics.weekly", 0)
	expectDenied(ctx, t, scoped, "SELECT count(*) FROM public.orders")
	expectDenied(ctx, t, scoped, "INSERT INTO analytics.daily VALUES (2, 20)")
	_ = scoped.Close(ctx)

	// A missing schema is reported, never granted as something wider.
	missing := roles.ApplyScope(ctx, admin, role, oramav1alpha1.ConnectionCredentialScope{Access: "read", Schema: "nowhere"})
	if !errors.Is(missing, errConnectionSchemaMissing) {
		t.Fatalf("missing schema error = %v", missing)
	}

	// Rotation: the old password stops working, the new one works.
	old := password
	password = issue(oramav1alpha1.ConnectionCredentialScope{Access: oramav1alpha1.ConnectionAccessRead})
	if conn, err := tryConnect(ctx, admin, role, old); err == nil {
		_ = conn.Close(ctx)
		t.Fatal("the rotated-out password still signs in")
	}
	open := connectAs(ctx, t, admin, role, password)
	expectRows(ctx, t, open, "SELECT count(*) FROM public.orders", 2)

	// Revocation: the open session ends and no new one starts.
	if err := roles.DropRole(ctx, admin, role); err != nil {
		t.Fatal(err)
	}
	if _, err := open.Exec(ctx, "SELECT 1"); err == nil {
		t.Fatal("a session of the revoked role still runs queries")
	}
	if conn, err := tryConnect(ctx, admin, role, password); err == nil {
		_ = conn.Close(ctx)
		t.Fatal("the revoked role still signs in")
	}
	if exists, err := roles.RoleExists(ctx, admin, role); err != nil || exists {
		t.Fatalf("role exists after revocation = %v, %v", exists, err)
	}
	if err := roles.DropRole(ctx, admin, role); err != nil {
		t.Fatalf("dropping a dropped role: %v", err)
	}
	// The operator never touches a role that is not a connection role.
	if err := roles.DropRole(ctx, admin, "app"); err == nil {
		t.Fatal("the owner role could be dropped")
	}
}

// startThrowawayPostgres runs initdb and postgres in a temporary directory with SCRAM for TCP
// sign-ins and returns the superuser target. The cluster is stopped and deleted on cleanup.
func startThrowawayPostgres(t *testing.T) ConnectionPostgresTarget {
	t.Helper()
	bin := os.Getenv("PG_BIN")
	tool := func(name string) string {
		if bin != "" {
			return filepath.Join(bin, name)
		}
		path, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s is not on PATH; set PG_BIN", name)
		}
		return path
	}
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	pwfile := filepath.Join(dir, "pw")
	const superPassword = "throwaway-superuser"
	if err := os.WriteFile(pwfile, []byte(superPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(tool("initdb"), "-D", data, "-U", "postgres", "--pwfile", pwfile,
		"--auth-host=scram-sha-256", "--auth-local=scram-sha-256", "--no-instructions").CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v\n%s", err, output)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	// TCP only: a temporary directory's path is too long for a Unix socket on macOS.
	options := fmt.Sprintf("-p %d -c listen_addresses=127.0.0.1 -c unix_socket_directories=''", port)
	if output, err := exec.Command(tool("pg_ctl"), "-D", data, "-o", options, "-l", filepath.Join(dir, "log"), "-w", "start").CombinedOutput(); err != nil {
		log, _ := os.ReadFile(filepath.Join(dir, "log"))
		t.Fatalf("pg_ctl start: %v\n%s\n%s", err, output, log)
	}
	t.Cleanup(func() {
		_ = exec.Command(tool("pg_ctl"), "-D", data, "-m", "immediate", "-w", "stop").Run()
	})
	return ConnectionPostgresTarget{Host: "127.0.0.1", Port: port, Database: "postgres", User: "postgres", Password: superPassword}
}

func tryConnect(ctx context.Context, target ConnectionPostgresTarget, user, password string) (*pgx.Conn, error) {
	uri := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, password),
		Host:     net.JoinHostPort(target.Host, strconv.Itoa(target.Port)),
		Path:     "/" + target.Database,
		RawQuery: "sslmode=disable",
	}
	return pgx.Connect(ctx, uri.String())
}

func connectAs(ctx context.Context, t *testing.T, target ConnectionPostgresTarget, user, password string) *pgx.Conn {
	t.Helper()
	conn, err := tryConnect(ctx, target, user, password)
	if err != nil {
		t.Fatalf("sign in as %s: %v", user, err)
	}
	return conn
}

func mustExec(ctx context.Context, t *testing.T, conn *pgx.Conn, statement string) {
	t.Helper()
	if _, err := conn.Exec(ctx, statement); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

func expectRows(ctx context.Context, t *testing.T, conn *pgx.Conn, query string, want int64) {
	t.Helper()
	var got int64
	if err := conn.QueryRow(ctx, query).Scan(&got); err != nil || got != want {
		t.Fatalf("%s = %d, %v; want %d", query, got, err, want)
	}
}

func expectDenied(ctx context.Context, t *testing.T, conn *pgx.Conn, statement string) {
	t.Helper()
	_, err := conn.Exec(ctx, statement)
	if err == nil || !strings.Contains(err.Error(), "permission denied") && !strings.Contains(err.Error(), "must be owner") {
		t.Fatalf("%s was not refused: %v", statement, err)
	}
}
