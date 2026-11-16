/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/pbkdf2"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Postgres roles for credential-scoped connections (operator#469, design decision D4). The operator
// signs in to the target service's CloudNativePG cluster as its superuser and keeps one LOGIN role
// per connection. The superuser credential is the operator's alone: it is read from the
// CloudNativePG-generated <cluster>-superuser Secret and is never delivered to a workload.

const (
	// connectionRolePrefix names every per-connection role, so a role this operator made is
	// recognisable and can never be the service's own owner role ("app").
	connectionRolePrefix = "lake_conn_"
	// connectionPostgresTimeout bounds one role operation against one service.
	connectionPostgresTimeout = 15 * time.Second
	// scramIterations matches PostgreSQL's default scram_iterations.
	scramIterations = 4096
	// trustedSearchPath is the only search_path a superuser session against a service uses
	// (operator#800, LSR-138). The service's database belongs to the tenant, and its owner may set
	// the database's default search_path and create objects in its own schemas; a superuser session
	// that resolved a name through that path would run the owner's look-alike of a catalog view,
	// function or operator with superuser rights. pg_temp is listed last so a temporary object can
	// never come first either.
	trustedSearchPath = "pg_catalog, pg_temp"
)

// errConnectionSchemaMissing is returned when a schema-scoped credential names a schema the
// service's database does not have yet. The connection stays Pending until it exists.
var errConnectionSchemaMissing = errors.New("schema does not exist")

// ConnectionPostgresTarget is where and as whom the operator manages a service's roles.
type ConnectionPostgresTarget struct {
	Host     string
	Port     int
	Database string
	User     string
	Password string
	// CA is the PEM of the CNPG cluster's CA (Secret <cluster>-ca, key ca.crt). The operator verifies
	// the server against it and the host name before sending the superuser password
	// (orama-operator#775); without it no session is opened.
	CA []byte
}

// PostgresRoleManager is the injected dependency through which the Connection controller manages
// per-connection roles. The pgx implementation talks to the service; tests substitute a fake.
type PostgresRoleManager interface {
	// RoleExists reports whether the role exists and may log in.
	RoleExists(ctx context.Context, target ConnectionPostgresTarget, role string) (bool, error)
	// SetRole creates the role, or updates it, as a LOGIN role with exactly this SCRAM verifier and
	// exactly the grants of scope, in one transaction.
	SetRole(ctx context.Context, target ConnectionPostgresTarget, role, verifier string, scope oramav1alpha1.ConnectionCredentialScope) error
	// ApplyScope replaces the role's grants with exactly the grants of scope, in one transaction.
	ApplyScope(ctx context.Context, target ConnectionPostgresTarget, role string, scope oramav1alpha1.ConnectionCredentialScope) error
	// DropRole stops new sign-ins, ends the role's sessions, removes its grants and drops it. A
	// missing role is not an error.
	DropRole(ctx context.Context, target ConnectionPostgresTarget, role string) error
}

// connectionRoleName is the Postgres role of one connection: the prefix and the row UUID without
// dashes, 42 characters, inside the 63-byte identifier limit.
func connectionRoleName(connectionID string) string {
	return connectionRolePrefix + strings.ReplaceAll(strings.ToLower(connectionID), "-", "")
}

// newConnectionPassword returns 32 random bytes as hex: no character needs quoting anywhere it is
// written (a URI, an environment variable, a .pgpass line).
func newConnectionPassword() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// scramSHA256Verifier computes the verifier PostgreSQL stores for a password (RFC 5802, RFC 7677),
// so the plaintext password never reaches the server, its statement log or pg_stat_activity.
func scramSHA256Verifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return scramSHA256VerifierWithSalt(password, salt, scramIterations), nil
}

func scramSHA256VerifierWithSalt(password string, salt []byte, iterations int) string {
	salted := pbkdf2.Key([]byte(password), salt, iterations, sha256.Size, sha256.New)
	clientKey := scramHMAC(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := scramHMAC(salted, "Server Key")
	encode := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, encode(salt), encode(storedKey[:]), encode(serverKey))
}

func scramHMAC(key []byte, message string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(message))
	return mac.Sum(nil)
}

// connectionScopeStatements are the grants of one scope. They run after the role's previous
// grants are removed, in the same transaction, so a scope change is atomic for other sessions.
func connectionScopeStatements(role string, scope oramav1alpha1.ConnectionCredentialScope, owner string) []string {
	r := pgx.Identifier{role}.Sanitize()
	if scope.Schema == "" {
		statements := []string{"GRANT pg_read_all_data TO " + r}
		if scope.Access == oramav1alpha1.ConnectionAccessWrite {
			statements = append(statements, "GRANT pg_write_all_data TO "+r)
		}
		return statements
	}
	s := pgx.Identifier{scope.Schema}.Sanitize()
	o := pgx.Identifier{owner}.Sanitize()
	tables := "SELECT"
	sequences := "USAGE, SELECT"
	if scope.Access == oramav1alpha1.ConnectionAccessWrite {
		tables = "SELECT, INSERT, UPDATE, DELETE"
		sequences = "USAGE, SELECT, UPDATE"
	}
	return []string{
		"GRANT USAGE ON SCHEMA " + s + " TO " + r,
		"GRANT " + tables + " ON ALL TABLES IN SCHEMA " + s + " TO " + r,
		"GRANT " + sequences + " ON ALL SEQUENCES IN SCHEMA " + s + " TO " + r,
		// Tables the owner creates later are covered too.
		"ALTER DEFAULT PRIVILEGES FOR ROLE " + o + " IN SCHEMA " + s + " GRANT " + tables + " ON TABLES TO " + r,
		"ALTER DEFAULT PRIVILEGES FOR ROLE " + o + " IN SCHEMA " + s + " GRANT " + sequences + " ON SEQUENCES TO " + r,
	}
}

// pgxPostgresRoleManager is the PostgresRoleManager that signs in to the service over pgx.
type pgxPostgresRoleManager struct{}

// connectionPostgresConfig is the session the operator opens as a service's superuser. The
// search_path is a startup parameter, so it is in force before the first statement and overrides
// the database's own default (ALTER DATABASE ... SET search_path), which the tenant controls.
func connectionPostgresConfig(target ConnectionPostgresTarget) (*pgx.ConnConfig, error) {
	uri := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(target.User, target.Password),
		Host:   net.JoinHostPort(target.Host, strconv.Itoa(target.Port)),
		Path:   "/" + target.Database,
	}
	// orama-operator#775: verify-full against the cluster's own CA and its host name, with no
	// plaintext or unverified fallback, so nothing on the path can read the superuser password.
	if len(target.CA) == 0 {
		return nil, errors.New("no CA for the service's Postgres: the operator never signs in without verifying the server")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(target.CA) {
		return nil, errors.New("the service's Postgres CA is not a PEM certificate")
	}
	query := url.Values{}
	query.Set("sslmode", "verify-full")
	query.Set("application_name", "orama-operator-connections")
	uri.RawQuery = query.Encode()
	config, err := pgx.ParseConfig(uri.String())
	if err != nil {
		return nil, err
	}
	config.TLSConfig = &tls.Config{RootCAs: roots, ServerName: target.Host, MinVersion: tls.VersionTLS12}
	config.Fallbacks = nil
	config.RuntimeParams["search_path"] = trustedSearchPath
	return config, nil
}

func (pgxPostgresRoleManager) connect(ctx context.Context, target ConnectionPostgresTarget) (*pgx.Conn, error) {
	config, err := connectionPostgresConfig(target)
	if err != nil {
		return nil, err
	}
	return pgx.ConnectConfig(ctx, config)
}

// pinSearchPath sets the trusted search_path again for one transaction. The startup parameter
// already holds it; this keeps each transaction correct on its own, whatever the session did.
func pinSearchPath(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, "SET LOCAL search_path = "+trustedSearchPath)
	return err
}

func (m pgxPostgresRoleManager) RoleExists(ctx context.Context, target ConnectionPostgresTarget, role string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, connectionPostgresTimeout)
	defer cancel()
	conn, err := m.connect(ctx, target)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	return postgresRoleCanLogin(ctx, conn, role)
}

func (m pgxPostgresRoleManager) SetRole(
	ctx context.Context,
	target ConnectionPostgresTarget,
	role, verifier string,
	scope oramav1alpha1.ConnectionCredentialScope,
) error {
	ctx, cancel := context.WithTimeout(ctx, connectionPostgresTimeout)
	defer cancel()
	conn, err := m.connect(ctx, target)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if err := pinSearchPath(ctx, tx); err != nil {
			return err
		}
		if err := setPostgresRole(ctx, tx, role, verifier); err != nil {
			return err
		}
		return applyPostgresScope(ctx, tx, role, scope)
	})
}

func (m pgxPostgresRoleManager) ApplyScope(
	ctx context.Context,
	target ConnectionPostgresTarget,
	role string,
	scope oramav1alpha1.ConnectionCredentialScope,
) error {
	ctx, cancel := context.WithTimeout(ctx, connectionPostgresTimeout)
	defer cancel()
	conn, err := m.connect(ctx, target)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if err := pinSearchPath(ctx, tx); err != nil {
			return err
		}
		return applyPostgresScope(ctx, tx, role, scope)
	})
}

func (m pgxPostgresRoleManager) DropRole(ctx context.Context, target ConnectionPostgresTarget, role string) error {
	ctx, cancel := context.WithTimeout(ctx, connectionPostgresTimeout)
	defer cancel()
	conn, err := m.connect(ctx, target)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	return dropPostgresRole(ctx, conn, role)
}

func postgresRoleCanLogin(ctx context.Context, db interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, role string) (bool, error) {
	var canLogin bool
	err := db.QueryRow(ctx, `SELECT rolcanlogin FROM pg_catalog.pg_roles WHERE rolname OPERATOR(pg_catalog.=) $1`, role).Scan(&canLogin)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return canLogin, err
}

func setPostgresRole(ctx context.Context, tx pgx.Tx, role, verifier string) error {
	if !strings.HasPrefix(role, connectionRolePrefix) {
		return fmt.Errorf("refusing to manage role %q: not a connection role", role)
	}
	if !strings.HasPrefix(verifier, "SCRAM-SHA-256$") || strings.ContainsAny(verifier, "'\\") {
		return errors.New("refusing a password that is not a SCRAM-SHA-256 verifier")
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname OPERATOR(pg_catalog.=) $1)`, role).Scan(&exists); err != nil {
		return err
	}
	r := pgx.Identifier{role}.Sanitize()
	verb := "ALTER ROLE "
	if !exists {
		verb = "CREATE ROLE "
	}
	// A utility statement takes no parameters; the verifier is checked above to hold no quote.
	_, err := tx.Exec(ctx, verb+r+" WITH LOGIN INHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '"+verifier+"'")
	return err
}

func applyPostgresScope(ctx context.Context, tx pgx.Tx, role string, scope oramav1alpha1.ConnectionCredentialScope) error {
	if !strings.HasPrefix(role, connectionRolePrefix) {
		return fmt.Errorf("refusing to manage role %q: not a connection role", role)
	}
	var owner string
	if err := tx.QueryRow(ctx, `SELECT pg_catalog.pg_get_userbyid(datdba) FROM pg_catalog.pg_database WHERE datname OPERATOR(pg_catalog.=) pg_catalog.current_database()`).Scan(&owner); err != nil {
		return err
	}
	if scope.Schema != "" {
		var found bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname OPERATOR(pg_catalog.=) $1)`, scope.Schema).Scan(&found); err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: %s", errConnectionSchemaMissing, scope.Schema)
		}
	}
	r := pgx.Identifier{role}.Sanitize()
	// Start from nothing: DROP OWNED BY revokes every privilege and default privilege granted to
	// the role in this database; the predefined roles are memberships and are revoked by name.
	reset := []string{
		"DROP OWNED BY " + r,
		"REVOKE pg_read_all_data FROM " + r,
		"REVOKE pg_write_all_data FROM " + r,
	}
	for _, statement := range append(reset, connectionScopeStatements(role, scope, owner)...) {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("%s: %w", strings.SplitN(statement, " TO ", 2)[0], err)
		}
	}
	return nil
}

func dropPostgresRole(ctx context.Context, conn *pgx.Conn, role string) error {
	if !strings.HasPrefix(role, connectionRolePrefix) {
		return fmt.Errorf("refusing to drop role %q: not a connection role", role)
	}
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname OPERATOR(pg_catalog.=) $1)`, role).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	r := pgx.Identifier{role}.Sanitize()
	// NOLOGIN first, so no new session starts while the open ones are ended.
	if _, err := conn.Exec(ctx, "ALTER ROLE "+r+" NOLOGIN"); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `SELECT pg_catalog.pg_terminate_backend(pid) FROM pg_catalog.pg_stat_activity WHERE usename OPERATOR(pg_catalog.=) $1`, role); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if err := pinSearchPath(ctx, tx); err != nil {
			return err
		}
		for _, statement := range []string{"DROP OWNED BY " + r, "DROP ROLE " + r} {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	})
}
