// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	gocql "github.com/apache/cassandra-gocql-driver/v2"
	"github.com/go-sql-driver/mysql"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Tenant users (operator#771). TiDB Operator and K8ssandra make only an administrator: TiDB's root
// and the <cluster>-superuser role. A tenant must never receive either, so the Orama operator signs
// in as that administrator and keeps one non-administrator user per service, with every right on
// the tenant's own databases or keyspaces and none on the store itself (no users or roles, no
// system schemas, no global privileges). The administrator's password stays in the Secret only the
// operator reads; the services Secret carries the tenant user alone.
//
// The same pass migrates a service made before operator#771: its tenant user is created on the
// next reconcile, with rights on the databases or keyspaces the tenant already made while it held
// the administrator's credential (the cluster is that tenant's, and its data stays reachable), and
// the administrator's connection file is removed from the services Secret (deliversAdministrator).
//
// A user of the tenant user's name that the operator did not leave as it wants it (a superuser,
// granted a role, holding a right on anything but the tenant's own data, or on more hosts than
// '%') is dropped and made again: while a tenant held the administrator's credential it could
// have made one.

// tenantUserProvisioner signs in to a store as its administrator and makes sure the tenant user is
// there with only its rights. Tests replace tenantUsers.
type tenantUserProvisioner interface {
	ensureTiDB(ctx context.Context, in catalogService, rootPassword, tenantPassword string) error
	ensureCassandra(ctx context.Context, in catalogService, superuser, superPassword, tenantPassword string) error
	// rotateTiDBRoot and rotateCassandraSuperuser set the administrator's password to next,
	// signed in with current; when current no longer signs in but next does, the password was
	// already changed and they succeed without a change (operator#1172).
	rotateTiDBRoot(ctx context.Context, in catalogService, current, next string) error
	rotateCassandraSuperuser(ctx context.Context, in catalogService, superuser, current, next string) error
	// otherTiDBUsers and otherCassandraRoles list the users or roles that are neither the
	// administrator's nor the tenant user's (operator#1172).
	otherTiDBUsers(ctx context.Context, in catalogService, rootPassword string) ([]string, error)
	otherCassandraRoles(ctx context.Context, in catalogService, superuser, password string) ([]string, error)
}

var tenantUsers tenantUserProvisioner = inClusterTenantUsers{}

// tenantUsersEnsured remembers the services whose tenant user is in place, keyed by the service and
// a digest of the tenant user's password, so a steady service makes no call to its store. A
// restarted operator checks each one once again; a new password (a recreated service) is a new
// key. The administrator's password is not in the key: its rotation (operator#1172) does not
// change the tenant user.
var tenantUsersEnsured sync.Map

// ensureTenantUserOnce runs ensure unless it already succeeded for this service and this tenant
// password, and reports whether the tenant user is in place. A failure is logged and tried again
// on the next pass. Meanwhile no new connection is written; a tenant user's connection already in
// the services Secret stays, as for any transient failure, and an administrator's never does.
func ensureTenantUserOnce(ctx context.Context, in catalogService, ensure func(context.Context) error) bool {
	digest := sha256.Sum256([]byte(in.tenantPassword))
	key := in.svc.Kind + "/" + in.namespace + "/" + in.name + "/" + hex.EncodeToString(digest[:8])
	if _, done := tenantUsersEnsured.Load(key); done {
		return true
	}
	callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := ensure(callCtx); err != nil {
		logf.FromContext(ctx).Error(err, "the tenant user is not in place yet; tried again on the next pass",
			"namespace", in.namespace, "service", in.svc.Slug, "kind", in.svc.Kind)
		return false
	}
	tenantUsersEnsured.Store(key, true)
	return true
}

type inClusterTenantUsers struct{}

// --- TiDB ---

// tidbSystemDatabases are TiDB's own schemas, on which the tenant user never gets a right.
var tidbSystemDatabases = map[string]bool{
	"mysql": true, "information_schema": true, "performance_schema": true, "metrics_schema": true, "sys": true,
}

// mysqlString quotes s as a MySQL string literal.
func mysqlString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(s) + "'"
}

// mysqlIdentifier quotes s as a MySQL identifier.
func mysqlIdentifier(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

func tidbTenantAccount(host string) string {
	return mysqlString(tidbTenantUser) + "@" + mysqlString(host)
}

// tidbTenantCreateStatements create the tenant user with its own database and give it every
// privilege on each of databases, and nothing else. Each is idempotent; the password is written
// as its mysql_native_password hash.
func tidbTenantCreateStatements(password string, databases []string) []string {
	account := tidbTenantAccount("%")
	identified := fmt.Sprintf("IDENTIFIED WITH 'mysql_native_password' AS '%s'", tidbNativePasswordHash(password))
	statements := []string{
		"CREATE DATABASE IF NOT EXISTS " + mysqlIdentifier(tidbTenantDatabase),
		"CREATE USER IF NOT EXISTS " + account + " " + identified,
		"ALTER USER " + account + " " + identified,
	}
	for _, database := range databases {
		statements = append(statements, "GRANT ALL PRIVILEGES ON "+mysqlIdentifier(database)+".* TO "+account)
	}
	return statements
}

// tidbTenantDatabases are the databases the tenant user gets every privilege on: its own, and every
// one that is not TiDB's (a tenant that held root may have made some, and its data stays its own).
func tidbTenantDatabases(databases []string) []string {
	seen := map[string]bool{tidbTenantDatabase: true}
	out := []string{tidbTenantDatabase}
	for _, database := range databases {
		if tidbSystemDatabases[strings.ToLower(database)] || seen[database] {
			continue
		}
		seen[database] = true
		out = append(out, database)
	}
	sort.Strings(out[1:])
	return out
}

// tidbTenantGrantsAcceptable: the tenant user exists only as 'app'@'%', and SHOW GRANTS lists
// nothing but USAGE and ALL PRIVILEGES on databases that are not TiDB's. Any other grant (a global
// or system-schema privilege, a role, WITH GRANT OPTION) means the user is not the operator's.
func tidbTenantGrantsAcceptable(hosts, grants []string) bool {
	if len(hosts) != 1 || hosts[0] != "%" {
		return false
	}
	suffix := " to " + strings.ToLower(tidbTenantAccount("%"))
	for _, grant := range grants {
		line := strings.Join(strings.Fields(strings.ToLower(strings.ReplaceAll(grant, "`", ""))), " ")
		if line == "grant usage on *.*"+suffix {
			continue
		}
		database, ok := strings.CutPrefix(line, "grant all privileges on ")
		if !ok {
			return false
		}
		database, ok = strings.CutSuffix(database, ".*"+suffix)
		if !ok || database == "*" || database == "" || strings.ContainsAny(database, " '") || tidbSystemDatabases[database] {
			return false
		}
	}
	return true
}

// tidbTenantUserStatements is what the operator runs as root, given the hosts the tenant user
// exists on, its grants (read only when it exists on '%' alone) and the cluster's databases.
func tidbTenantUserStatements(hosts, grants, databases []string, password string) []string {
	var statements []string
	if len(hosts) > 0 && !tidbTenantGrantsAcceptable(hosts, grants) {
		for _, host := range hosts {
			statements = append(statements, "DROP USER IF EXISTS "+tidbTenantAccount(host))
		}
	}
	return append(statements, tidbTenantCreateStatements(password, tidbTenantDatabases(databases))...)
}

// statementVerb names a statement in an error without its arguments, which can carry a hash.
func statementVerb(statement string) string {
	words := strings.Fields(statement)
	if len(words) > 2 {
		words = words[:2]
	}
	return strings.Join(words, " ")
}

// ensureTiDB signs in as root over TiDB's SQL port (workspace NetworkPolicy
// allow-orama-operator-tidb-sql admits the operator) and runs tidbTenantUserStatements.
func (inClusterTenantUsers) ensureTiDB(ctx context.Context, in catalogService, rootPassword, tenantPassword string) error {
	conn, done, err := tidbRootSession(ctx, in, rootPassword)
	if err != nil {
		return err
	}
	defer done()
	hosts, err := queryTiDBStrings(ctx, conn, "SELECT Host FROM mysql.user WHERE User = ?", tidbTenantUser)
	if err != nil {
		return err
	}
	databases, err := queryTiDBStrings(ctx, conn, "SHOW DATABASES")
	if err != nil {
		return err
	}
	var grants []string
	if len(hosts) == 1 && hosts[0] == "%" {
		if grants, err = queryTiDBStrings(ctx, conn, "SHOW GRANTS FOR "+tidbTenantAccount("%")); err != nil {
			return err
		}
	}
	for _, statement := range tidbTenantUserStatements(hosts, grants, databases, tenantPassword) {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("run %s on TiDB: %w", statementVerb(statement), err)
		}
	}
	return nil
}

// tidbRootSession opens one SQL session to the service as root; done closes it.
func tidbRootSession(ctx context.Context, in catalogService, rootPassword string) (*sql.Conn, func(), error) {
	config := mysql.NewConfig()
	config.User, config.Passwd = tidbRootUser, rootPassword
	config.Net, config.Addr = "tcp", tidbServiceKind{}.endpoint(in)
	config.Timeout, config.ReadTimeout, config.WriteTimeout = 10*time.Second, 30*time.Second, 30*time.Second
	connector, err := mysql.NewConnector(config)
	if err != nil {
		return nil, nil, err
	}
	db := sql.OpenDB(connector)
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("sign in to TiDB as root: %w", err)
	}
	return conn, func() { _ = conn.Close(); _ = db.Close() }, nil
}

// tidbRootPasswordStatement sets root's password from its hash.
func tidbRootPasswordStatement(password string) string {
	return fmt.Sprintf("ALTER USER 'root'@'%%' IDENTIFIED WITH 'mysql_native_password' AS '%s'", tidbNativePasswordHash(password))
}

func (inClusterTenantUsers) rotateTiDBRoot(ctx context.Context, in catalogService, current, next string) error {
	conn, done, err := tidbRootSession(ctx, in, current)
	if err != nil {
		// Changed already, by a pass that stopped before it could record it.
		if _, nextDone, nextErr := tidbRootSession(ctx, in, next); nextErr == nil {
			nextDone()
			return nil
		}
		return err
	}
	defer done()
	if _, err := conn.ExecContext(ctx, tidbRootPasswordStatement(next)); err != nil {
		return fmt.Errorf("run ALTER USER on TiDB: %w", err)
	}
	return nil
}

func (inClusterTenantUsers) otherTiDBUsers(ctx context.Context, in catalogService, rootPassword string) ([]string, error) {
	conn, done, err := tidbRootSession(ctx, in, rootPassword)
	if err != nil {
		return nil, err
	}
	defer done()
	rows, err := conn.QueryContext(ctx, "SELECT User, Host FROM mysql.user")
	if err != nil {
		return nil, fmt.Errorf("run SELECT on TiDB: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var accounts [][2]string
	for rows.Next() {
		var user, host string
		if err := rows.Scan(&user, &host); err != nil {
			return nil, err
		}
		accounts = append(accounts, [2]string{user, host})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tidbOtherUsers(accounts), nil
}

// tidbOtherUsers names, as user@host and sorted, every account but root and the tenant user.
func tidbOtherUsers(accounts [][2]string) []string {
	var out []string
	for _, account := range accounts {
		if account[0] == tidbRootUser || account[0] == tidbTenantUser {
			continue
		}
		out = append(out, account[0]+"@"+account[1])
	}
	sort.Strings(out)
	return out
}

// queryTiDBStrings reads the first column of every row.
func queryTiDBStrings(ctx context.Context, conn *sql.Conn, query string, args ...interface{}) ([]string, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("run %s on TiDB: %w", statementVerb(query), err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		values := make([]sql.RawBytes, len(columns))
		dest := make([]interface{}, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, string(values[0]))
	}
	return out, rows.Err()
}

// --- Cassandra ---

const (
	// cassandraTenantRole is the role a tenant receives: LOGIN, not a superuser, with rights on
	// its own keyspaces only (operator#771). cassandraTenantKeyspace is the keyspace made for it.
	cassandraTenantRole     = "app"
	cassandraTenantKeyspace = "app"
	// cassandraTenantKey is the key of the tenant role's password in Secret <name>-tenant.
	cassandraTenantKey = "password"
	// cassandraMaxReplication caps the tenant keyspace's replication factor.
	cassandraMaxReplication = 3
)

// cassandraTenantPermissions are the rights the tenant role gets on each of its keyspaces: what an
// application needs to make, change, read and write its tables. AUTHORIZE is left out, so it
// cannot hand rights to other roles; it cannot make roles at all.
var cassandraTenantPermissions = []string{"CREATE", "ALTER", "DROP", "SELECT", "MODIFY"}

// cassandraSystemKeyspace: Cassandra's own keyspaces and the ones K8ssandra's components make
// (Reaper, Stargate's auth). The tenant role never gets a right on them.
func cassandraSystemKeyspace(keyspace string) bool {
	return strings.HasPrefix(keyspace, "system") || keyspace == "reaper_db" || keyspace == "data_endpoint_auth"
}

// cqlString quotes s as a CQL string literal; cqlIdentifier quotes s as a CQL identifier.
func cqlString(s string) string     { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
func cqlIdentifier(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// cassandraTenantRoleAcceptable: the role is not a superuser, is granted no role, and every right
// it holds is on a keyspace that is not the system's, or a table in one ("data/<keyspace>" and
// "data/<keyspace>/<table>" in system_auth.role_permissions).
func cassandraTenantRoleAcceptable(superuser bool, memberOf, resources []string) bool {
	if superuser || len(memberOf) > 0 {
		return false
	}
	for _, resource := range resources {
		rest, ok := strings.CutPrefix(resource, "data/")
		if !ok {
			return false
		}
		keyspace, _, _ := strings.Cut(rest, "/")
		if keyspace == "" || cassandraSystemKeyspace(keyspace) {
			return false
		}
	}
	return true
}

// cassandraTenantRoleStatements is what the operator runs as the superuser, given whether the
// tenant role exists and what it holds, and the cluster's keyspaces.
func cassandraTenantRoleStatements(exists, superuser bool, memberOf, resources, keyspaces []string, instances int32, password string) []string {
	var statements []string
	if exists && !cassandraTenantRoleAcceptable(superuser, memberOf, resources) {
		statements = append(statements, "DROP ROLE IF EXISTS "+cqlIdentifier(cassandraTenantRole))
	}
	replication := int(instances)
	if replication > cassandraMaxReplication {
		replication = cassandraMaxReplication
	}
	if replication < 1 {
		replication = 1
	}
	role := cqlIdentifier(cassandraTenantRole)
	options := "WITH PASSWORD = " + cqlString(password) + " AND LOGIN = true AND SUPERUSER = false"
	statements = append(statements,
		fmt.Sprintf("CREATE KEYSPACE IF NOT EXISTS %s WITH replication = {'class': 'NetworkTopologyStrategy', %s: %d}",
			cqlIdentifier(cassandraTenantKeyspace), cqlString(cassandraDatacenter), replication),
		"CREATE ROLE IF NOT EXISTS "+role+" "+options,
		"ALTER ROLE "+role+" "+options,
	)
	for _, keyspace := range cassandraTenantKeyspaces(keyspaces) {
		for _, permission := range cassandraTenantPermissions {
			statements = append(statements, "GRANT "+permission+" ON KEYSPACE "+cqlIdentifier(keyspace)+" TO "+role)
		}
	}
	return statements
}

// cassandraTenantKeyspaces: the tenant's own keyspace, and every one that is not the system's (a
// tenant that held the superuser may have made some, and its data stays its own).
func cassandraTenantKeyspaces(keyspaces []string) []string {
	seen := map[string]bool{cassandraTenantKeyspace: true}
	out := []string{cassandraTenantKeyspace}
	for _, keyspace := range keyspaces {
		if cassandraSystemKeyspace(keyspace) || seen[keyspace] {
			continue
		}
		seen[keyspace] = true
		out = append(out, keyspace)
	}
	sort.Strings(out[1:])
	return out
}

// ensureCassandra signs in as the generated superuser over CQL (workspace NetworkPolicy
// allow-orama-operator-cassandra-cql admits the operator) and runs cassandraTenantRoleStatements.
// The management API cannot do this: it creates and drops roles but has no GRANT.
func (inClusterTenantUsers) ensureCassandra(ctx context.Context, in catalogService, superuser, superPassword, tenantPassword string) error {
	session, err := cassandraSession(in, superuser, superPassword)
	if err != nil {
		return err
	}
	defer session.Close()

	exists, isSuperuser := true, false
	var memberOf []string
	err = session.Query("SELECT is_superuser, member_of FROM system_auth.roles WHERE role = ?", cassandraTenantRole).
		ScanContext(ctx, &isSuperuser, &memberOf)
	switch {
	case errors.Is(err, gocql.ErrNotFound):
		exists = false
	case err != nil:
		return fmt.Errorf("read role %s: %w", cassandraTenantRole, err)
	}
	var resources []string
	if exists {
		if resources, err = cassandraStrings(ctx, session, "SELECT resource FROM system_auth.role_permissions WHERE role = ?", cassandraTenantRole); err != nil {
			return err
		}
	}
	keyspaces, err := cassandraStrings(ctx, session, "SELECT keyspace_name FROM system_schema.keyspaces")
	if err != nil {
		return err
	}
	for _, statement := range cassandraTenantRoleStatements(exists, isSuperuser, memberOf, resources, keyspaces, in.spec.Instances, tenantPassword) {
		if err := session.Query(statement).ExecContext(ctx); err != nil {
			return fmt.Errorf("run %s on Cassandra: %w", statementVerb(statement), err)
		}
	}
	return nil
}

// cassandraSession opens a CQL session to the datacenter as user.
func cassandraSession(in catalogService, user, password string) (*gocql.Session, error) {
	cluster := gocql.NewCluster(cassandraHost(in))
	cluster.Port = cassandraPort
	cluster.Authenticator = gocql.PasswordAuthenticator{Username: user, Password: password}
	// Only the datacenter Service's own addresses; the pods behind it are the whole cluster.
	cluster.DisableInitialHostLookup = true
	cluster.ConnectTimeout, cluster.Timeout = 10*time.Second, 30*time.Second
	cluster.NumConns = 1
	cluster.Consistency = gocql.LocalQuorum
	session, err := cluster.CreateSession()
	if err != nil {
		return nil, fmt.Errorf("sign in to Cassandra as %s: %w", user, err)
	}
	return session, nil
}

func (inClusterTenantUsers) rotateCassandraSuperuser(ctx context.Context, in catalogService, superuser, current, next string) error {
	session, err := cassandraSession(in, superuser, current)
	if err != nil {
		// Changed already, by a pass that stopped before it could record it.
		if nextSession, nextErr := cassandraSession(in, superuser, next); nextErr == nil {
			nextSession.Close()
			return nil
		}
		return err
	}
	defer session.Close()
	statement := "ALTER ROLE " + cqlIdentifier(superuser) + " WITH PASSWORD = " + cqlString(next)
	if err := session.Query(statement).ExecContext(ctx); err != nil {
		return fmt.Errorf("run ALTER ROLE on Cassandra: %w", err)
	}
	return nil
}

func (inClusterTenantUsers) otherCassandraRoles(ctx context.Context, in catalogService, superuser, password string) ([]string, error) {
	session, err := cassandraSession(in, superuser, password)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	roles, err := cassandraStrings(ctx, session, "SELECT role FROM system_auth.roles")
	if err != nil {
		return nil, err
	}
	return cassandraOtherRoles(roles, superuser), nil
}

// cassandraOtherRoles names, sorted, every role but the superuser, the tenant role and Cassandra's
// stock `cassandra`, which the operator drops itself (operator#772).
func cassandraOtherRoles(roles []string, superuser string) []string {
	var out []string
	for _, role := range roles {
		if role == superuser || role == cassandraTenantRole || role == "cassandra" {
			continue
		}
		out = append(out, role)
	}
	sort.Strings(out)
	return out
}

func cassandraStrings(ctx context.Context, session *gocql.Session, query string, args ...interface{}) ([]string, error) {
	iter := session.Query(query, args...).IterContext(ctx)
	var out []string
	var value string
	for iter.Scan(&value) {
		out = append(out, value)
	}
	if err := iter.Close(); err != nil {
		return nil, fmt.Errorf("run %s on Cassandra: %w", statementVerb(query), err)
	}
	return out, nil
}
