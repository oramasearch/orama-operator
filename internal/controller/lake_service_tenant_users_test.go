// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// tenantUserCall is one call the operator made to keep a tenant user.
type tenantUserCall struct {
	service, admin, adminPassword, tenantPassword string
}

// fakeTenantUsers records what the operator would run against TiDB and Cassandra (operator#771).
type fakeTenantUsers struct {
	err       error
	tidb      []tenantUserCall
	cassandra []tenantUserCall
}

func (f *fakeTenantUsers) ensureTiDB(_ context.Context, in catalogService, rootPassword, tenantPassword string) error {
	f.tidb = append(f.tidb, tenantUserCall{in.name, tidbRootUser, rootPassword, tenantPassword})
	return f.err
}

func (f *fakeTenantUsers) ensureCassandra(_ context.Context, in catalogService, superuser, superPassword, tenantPassword string) error {
	f.cassandra = append(f.cassandra, tenantUserCall{in.name, superuser, superPassword, tenantPassword})
	return f.err
}

func forgetEnsuredTenantUsers() {
	tenantUsersEnsured.Range(func(key, _ interface{}) bool {
		tenantUsersEnsured.Delete(key)
		return true
	})
}

func catalogCaseOf(t *testing.T, kind string) catalogCase {
	t.Helper()
	for _, c := range catalogCases() {
		if c.kind == kind {
			return c
		}
	}
	t.Fatalf("no catalog case for %s", kind)
	return catalogCase{}
}

// The delivered TiDB user is the tenant user, made by the operator signing in as root, and a
// steady service makes no further call.
func TestTiDBDeliversTheTenantUserNeverRoot(t *testing.T) {
	c := catalogCaseOf(t, serviceKindTiDB)
	h, _ := readyCatalogService(t, c)
	root, tenant := h.password(c), h.deliveredPassword(c)
	if len(h.tenantUsers.tidb) != 1 {
		t.Fatalf("tenant user calls = %+v, want one", h.tenantUsers.tidb)
	}
	if call := h.tenantUsers.tidb[0]; call.service != c.name() || call.adminPassword != root || call.tenantPassword != tenant {
		t.Fatalf("tenant user call = %+v", call)
	}
	credential := string(h.credentials()[c.slug+".json"])
	if !strings.Contains(credential, `"user":"app"`) || strings.Contains(credential, root) || strings.Contains(credential, `"root"`) {
		t.Fatalf("delivered credential is not the tenant user's: %s", credential)
	}
	h.poll()
	h.poll()
	if len(h.tenantUsers.tidb) != 1 {
		t.Fatalf("a steady service signed in to TiDB again: %+v", h.tenantUsers.tidb)
	}
}

// The delivered Cassandra role is the tenant role, made by the operator signing in as the generated
// superuser, and a steady service makes no further call.
func TestCassandraDeliversTheTenantRoleNeverTheSuperuser(t *testing.T) {
	c := catalogCaseOf(t, serviceKindCassandra)
	h, _ := readyCatalogService(t, c)
	tenant := h.deliveredPassword(c)
	if len(h.tenantUsers.cassandra) != 1 {
		t.Fatalf("tenant role calls = %+v, want one", h.tenantUsers.cassandra)
	}
	if call := h.tenantUsers.cassandra[0]; call.admin != c.slug+"-superuser" || call.adminPassword != "cql-secret" || call.tenantPassword != tenant {
		t.Fatalf("tenant role call = %+v", call)
	}
	credential := string(h.credentials()[c.slug+".json"])
	if !strings.Contains(credential, `"user":"app"`) || !strings.Contains(credential, `"keyspace":"app"`) ||
		strings.Contains(credential, "superuser") || strings.Contains(credential, "cql-secret") {
		t.Fatalf("delivered credential is not the tenant role's: %s", credential)
	}
	h.poll()
	if len(h.tenantUsers.cassandra) != 1 {
		t.Fatalf("a steady service signed in to Cassandra again: %+v", h.tenantUsers.cassandra)
	}
}

// A service made before operator#771 has its administrator's connection in the services Secret.
// The next pass removes it even while the tenant user cannot be made yet, and delivers the tenant
// user once it can.
func TestAnAdministratorConnectionIsRemovedFromTheServicesSecret(t *testing.T) {
	for _, kind := range []string{serviceKindTiDB, serviceKindCassandra} {
		t.Run(kind, func(t *testing.T) {
			c := catalogCaseOf(t, kind)
			h, store := readyCatalogService(t, c)
			admin := `{"kind":"tidb","user":"root","password":"` + h.password(c) + `"}`
			if kind == serviceKindCassandra {
				admin = `{"kind":"cassandra","user":"` + c.slug + `-superuser","password":"cql-secret"}`
			}
			secret := h.secret(lakeServicesSecret("prod"))
			secret.Data[c.slug+".json"] = []byte(admin)
			secret.Data[c.id] = []byte(admin)
			h.update(secret)

			// The operator restarts (nothing is remembered) and cannot reach the store yet.
			forgetEnsuredTenantUsers()
			h.tenantUsers.err = errors.New("connection refused")
			h.poll()
			for _, key := range []string{c.slug + ".json", c.id} {
				if value, found := h.credentials()[key]; found {
					t.Fatalf("the administrator's connection is still delivered under %s: %s", key, value)
				}
			}
			expectStatus(t, store.status(t, c.id), ServicePhaseProvisioning, "delivering the connection", c.endpoint)

			h.tenantUsers.err = nil
			h.poll()
			if got, want := string(h.credentials()[c.slug+".json"]), c.credential(h.deliveredPassword(c)); got != want {
				t.Fatalf("credential =\n%s\nwant\n%s", got, want)
			}
			expectStatus(t, store.status(t, c.id), ServicePhaseReady, "service ready", c.endpoint)
		})
	}
}

func TestDeliversAdministrator(t *testing.T) {
	admins := []string{"root"}
	for credential, want := range map[string]bool{
		`{"user":"root","password":"x"}`: true,
		`{"user":"app","password":"x"}`:  false,
		`{"password":"x"}`:               true,
		`not json`:                       true,
	} {
		if got := deliversAdministrator([]byte(credential), admins); got != want {
			t.Errorf("deliversAdministrator(%s) = %v, want %v", credential, got, want)
		}
	}
	if deliversAdministrator([]byte(`{"user":"root"}`), nil) {
		t.Error("a kind without administrators refused a connection")
	}
}

func TestTiDBTenantUserStatements(t *testing.T) {
	const password = "0123456789abcdef0123456789abcdef"
	hash := tidbNativePasswordHash(password)
	databases := []string{"INFORMATION_SCHEMA", "METRICS_SCHEMA", "PERFORMANCE_SCHEMA", "mysql", "sys", "test", "shop"}
	create := []string{
		"CREATE DATABASE IF NOT EXISTS `app`",
		"CREATE USER IF NOT EXISTS 'app'@'%' IDENTIFIED WITH 'mysql_native_password' AS '" + hash + "'",
		"ALTER USER 'app'@'%' IDENTIFIED WITH 'mysql_native_password' AS '" + hash + "'",
		"GRANT ALL PRIVILEGES ON `app`.* TO 'app'@'%'",
		"GRANT ALL PRIVILEGES ON `shop`.* TO 'app'@'%'",
		"GRANT ALL PRIVILEGES ON `test`.* TO 'app'@'%'",
	}
	// A cluster with no tenant user yet: it is made, with rights on the tenant's databases only.
	if got := tidbTenantUserStatements(nil, nil, databases, password); !reflect.DeepEqual(got, create) {
		t.Fatalf("statements =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(create, "\n"))
	}
	for _, statement := range create {
		if strings.Contains(statement, password) {
			t.Fatal("a statement carries the password itself")
		}
	}
	// The operator's own user is kept.
	own := []string{"GRANT USAGE ON *.* TO 'app'@'%'", "GRANT ALL PRIVILEGES ON `app`.* TO 'app'@'%'", "GRANT ALL PRIVILEGES ON `shop`.* TO 'app'@'%'"}
	if got := tidbTenantUserStatements([]string{"%"}, own, databases, password); !reflect.DeepEqual(got, create) {
		t.Fatalf("an acceptable user was changed:\n%s", strings.Join(got, "\n"))
	}
	// A user a tenant could have made while it held root is dropped and made again.
	for name, tc := range map[string]struct{ hosts, grants []string }{
		"global":       {[]string{"%"}, []string{"GRANT ALL PRIVILEGES ON *.* TO 'app'@'%'"}},
		"system":       {[]string{"%"}, []string{"GRANT USAGE ON *.* TO 'app'@'%'", "GRANT SELECT ON `mysql`.* TO 'app'@'%'"}},
		"grant option": {[]string{"%"}, []string{"GRANT ALL PRIVILEGES ON `app`.* TO 'app'@'%' WITH GRANT OPTION"}},
		"role":         {[]string{"%"}, []string{"GRANT 'admin'@'%' TO 'app'@'%'"}},
		"system ALL":   {[]string{"%"}, []string{"GRANT ALL PRIVILEGES ON `mysql`.* TO 'app'@'%'"}},
		"other host":   {[]string{"%", "10.0.0.%"}, nil},
	} {
		got := tidbTenantUserStatements(tc.hosts, tc.grants, databases, password)
		var drops []string
		for _, host := range tc.hosts {
			drops = append(drops, fmt.Sprintf("DROP USER IF EXISTS 'app'@'%s'", host))
		}
		if want := append(drops, create...); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: statements =\n%s", name, strings.Join(got, "\n"))
		}
	}
	// A host name is quoted, never spliced.
	if got := tidbTenantUserStatements([]string{`x' OR '1`}, nil, nil, password)[0]; got != `DROP USER IF EXISTS 'app'@'x'' OR ''1'` {
		t.Fatalf("drop = %s", got)
	}
}

func TestCassandraTenantRoleStatements(t *testing.T) {
	const password = "0123456789abcdef0123456789abcdef"
	keyspaces := []string{"system", "system_auth", "system_schema", "system_distributed", "system_traces", "system_views",
		"system_virtual_schema", "reaper_db", "data_endpoint_auth", "shop"}
	options := "WITH PASSWORD = '" + password + "' AND LOGIN = true AND SUPERUSER = false"
	create := []string{
		`CREATE KEYSPACE IF NOT EXISTS "app" WITH replication = {'class': 'NetworkTopologyStrategy', 'dc1': 3}`,
		`CREATE ROLE IF NOT EXISTS "app" ` + options,
		`ALTER ROLE "app" ` + options,
	}
	for _, keyspace := range []string{"app", "shop"} {
		for _, permission := range []string{"CREATE", "ALTER", "DROP", "SELECT", "MODIFY"} {
			create = append(create, fmt.Sprintf(`GRANT %s ON KEYSPACE "%s" TO "app"`, permission, keyspace))
		}
	}
	if got := cassandraTenantRoleStatements(false, false, nil, nil, keyspaces, 5, password); !reflect.DeepEqual(got, create) {
		t.Fatalf("statements =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(create, "\n"))
	}
	for _, statement := range create {
		if strings.Contains(statement, "AUTHORIZE") || strings.Contains(statement, "ALL KEYSPACES") || strings.Contains(statement, "system") {
			t.Fatalf("the tenant role gets more than its keyspaces: %s", statement)
		}
	}
	// One instance: replication 1.
	if got := cassandraTenantRoleStatements(false, false, nil, nil, nil, 1, password)[0]; !strings.HasSuffix(got, "'dc1': 1}") {
		t.Fatalf("keyspace = %s", got)
	}
	// The operator's own role is kept, with rights on the tenant's tables.
	if got := cassandraTenantRoleStatements(true, false, nil, []string{"data/app", "data/app/events", "data/shop"}, keyspaces, 3, password); !reflect.DeepEqual(got, create) {
		t.Fatalf("an acceptable role was changed:\n%s", strings.Join(got, "\n"))
	}
	// A role a tenant could have made while it held the superuser is dropped and made again.
	for name, tc := range map[string]struct {
		superuser bool
		memberOf  []string
		resources []string
	}{
		"superuser":     {superuser: true},
		"member of":     {memberOf: []string{"svc-999999999999-superuser"}},
		"all keyspaces": {resources: []string{"data"}},
		"system":        {resources: []string{"data/system_auth"}},
		"system table":  {resources: []string{"data/system_auth/roles"}},
		"roles":         {resources: []string{"roles"}},
		"a role":        {resources: []string{"roles/other"}},
		"functions":     {resources: []string{"functions"}},
	} {
		got := cassandraTenantRoleStatements(true, tc.superuser, tc.memberOf, tc.resources, keyspaces, 3, password)
		if want := append([]string{`DROP ROLE IF EXISTS "app"`}, create...); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: statements =\n%s", name, strings.Join(got, "\n"))
		}
	}
}
