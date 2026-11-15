// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// tenantUserCall is one call the operator made to keep a tenant user.
type tenantUserCall struct {
	service, admin, adminPassword, tenantPassword string
}

// fakeTenantUsers records what the operator would run against TiDB and Cassandra (operator#771,
// operator#1172).
type fakeTenantUsers struct {
	err       error
	tidb      []tenantUserCall
	cassandra []tenantUserCall
	// rotations records each administrator rotation as admin, current and next password;
	// rotateErr fails them. others is what the user listing finds.
	rotations []tenantUserCall
	rotateErr error
	others    []string
	listings  int
}

func (f *fakeTenantUsers) rotateTiDBRoot(_ context.Context, in catalogService, current, next string) error {
	f.rotations = append(f.rotations, tenantUserCall{in.name, tidbRootUser, current, next})
	return f.rotateErr
}

func (f *fakeTenantUsers) rotateCassandraSuperuser(_ context.Context, in catalogService, superuser, current, next string) error {
	f.rotations = append(f.rotations, tenantUserCall{in.name, superuser, current, next})
	return f.rotateErr
}

func (f *fakeTenantUsers) otherTiDBUsers(context.Context, catalogService, string) ([]string, error) {
	f.listings++
	return f.others, nil
}

func (f *fakeTenantUsers) otherCassandraRoles(context.Context, catalogService, string, string) ([]string, error) {
	f.listings++
	return f.others, nil
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
	tenant := h.deliveredPassword(c)
	if len(h.tenantUsers.tidb) != 1 {
		t.Fatalf("tenant user calls = %+v, want one", h.tenantUsers.tidb)
	}
	// Signed in with root's first password, which the same pass then rotated (operator#1172).
	root := h.tenantUsers.tidb[0].adminPassword
	if call := h.tenantUsers.tidb[0]; call.service != c.name() || len(root) != 32 || call.tenantPassword != tenant {
		t.Fatalf("tenant user call = %+v", call)
	}
	credential := string(h.credentials()[c.slug+".json"])
	if !strings.Contains(credential, `"user":"app"`) || strings.Contains(credential, root) || strings.Contains(credential, h.password(c)) || strings.Contains(credential, `"root"`) {
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
			secret := h.servicesSecret()
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

// servicesSecret is lake-prod-services, which stays in the Lake namespace while each service's own
// Secrets are in its workspace namespace (operator#778).
func (h *serviceHarness) servicesSecret() *corev1.Secret {
	h.t.Helper()
	secret := &corev1.Secret{}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testServiceNamespace, Name: lakeServicesSecret("prod")}, secret); err != nil {
		h.t.Fatalf("get secret %s: %v", lakeServicesSecret("prod"), err)
	}
	return secret
}

// Once the tenant user is delivered, the administrator's password is rotated once, the store first
// and then its Secret, and the rotation is recorded on the tenant Secret (operator#1172).
func TestTheAdministratorPasswordIsRotatedOnceTheTenantUserIsDelivered(t *testing.T) {
	for _, kind := range []string{serviceKindTiDB, serviceKindCassandra} {
		t.Run(kind, func(t *testing.T) {
			c := catalogCaseOf(t, kind)
			h, store := readyCatalogService(t, c)
			if len(h.tenantUsers.rotations) != 1 {
				t.Fatalf("rotations = %+v, want one", h.tenantUsers.rotations)
			}
			rotation := h.tenantUsers.rotations[0]
			adminSecret, adminKey, wantAdmin, wantCurrent := c.passwordSecret, c.passwordKey, tidbRootUser, h.tenantUsers.tidb[0].adminPassword
			if kind == serviceKindCassandra {
				adminSecret, adminKey, wantAdmin, wantCurrent = c.slug+"-superuser", "password", c.slug+"-superuser", "cql-secret"
			}
			if rotation.admin != wantAdmin || rotation.adminPassword != wantCurrent || len(rotation.tenantPassword) != 32 || rotation.tenantPassword == wantCurrent {
				t.Fatalf("rotation = %+v", rotation)
			}
			if got := string(h.secret(adminSecret).Data[adminKey]); got != rotation.tenantPassword {
				t.Fatal("the administrator's Secret does not hold the rotated password")
			}
			tenant := h.secret(c.tenantSecret)
			if tenant.Annotations[serviceAdminRotatedAnnotation] == "" {
				t.Fatal("the rotation is not recorded")
			}
			if _, found := tenant.Data[serviceAdminNextKey]; found {
				t.Fatal("the pending password was left on the tenant Secret")
			}
			expectStatus(t, store.status(t, c.id), ServicePhaseReady, "service ready", c.endpoint)
			h.poll()
			h.poll()
			if len(h.tenantUsers.rotations) != 1 || h.tenantUsers.listings != 1 {
				t.Fatalf("a rotated service called its store again: %d rotations, %d listings", len(h.tenantUsers.rotations), h.tenantUsers.listings)
			}
		})
	}
}

// A rotation that fails keeps the administrator's password and the pending one, holds back nothing,
// and finishes with the same new password on a later pass.
func TestAFailedAdministratorRotationIsFinishedLater(t *testing.T) {
	c := catalogCaseOf(t, serviceKindTiDB)
	store := &fakeServiceStore{services: []LakeService{testService(c.id, c.slug, c.kind, c.spec(`{"instances":3,"storage_gi":15}`), false)}}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.tenantUsers.rotateErr = errors.New("connection refused")
	h.poll()
	h.giveUIDs(c)
	c.markPartlyReady(h, c)
	c.markReady(h, c)
	h.poll()
	root := h.password(c)
	expectStatus(t, store.status(t, c.id), ServicePhaseReady, "service ready", c.endpoint)
	pending := string(h.secret(c.tenantSecret).Data[serviceAdminNextKey])
	if len(pending) != 32 || h.secret(c.tenantSecret).Annotations[serviceAdminRotatedAnnotation] != "" {
		t.Fatalf("pending = %q, annotations %v", pending, h.secret(c.tenantSecret).Annotations)
	}

	h.tenantUsers.rotateErr = nil
	h.poll()
	if last := h.tenantUsers.rotations[len(h.tenantUsers.rotations)-1]; last.adminPassword != root || last.tenantPassword != pending {
		t.Fatalf("retry = %+v, want %s -> %s", last, root, pending)
	}
	if h.password(c) != pending {
		t.Fatal("root's Secret does not hold the rotated password")
	}
}

// Users and roles the operator did not make are reported in the status message and kept.
func TestUnmanagedUsersAreReportedNotDropped(t *testing.T) {
	c := catalogCaseOf(t, serviceKindCassandra)
	store := &fakeServiceStore{services: []LakeService{testService(c.id, c.slug, c.kind, c.spec(`{"instances":3,"storage_gi":15}`), false)}}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.tenantUsers.others = []string{"analytics", "backdoor"}
	h.poll()
	h.giveUIDs(c)
	c.markPartlyReady(h, c)
	c.markReady(h, c)
	c.upstreamSecret(h, c)
	h.poll()
	expectStatus(t, store.status(t, c.id), ServicePhaseReady,
		"service ready; users and roles the operator did not make: analytics, backdoor", c.endpoint)
	if got := h.secret(c.tenantSecret).Annotations[serviceUnmanagedUsersAnnotation]; got != "analytics,backdoor" {
		t.Fatalf("recorded users = %q", got)
	}
	h.poll()
	expectStatus(t, store.status(t, c.id), ServicePhaseReady,
		"service ready; users and roles the operator did not make: analytics, backdoor", c.endpoint)
}

func TestOtherUsersExcludeTheAdministratorAndTheTenant(t *testing.T) {
	if got := tidbOtherUsers([][2]string{{"root", "%"}, {"app", "%"}, {"shop", "%"}, {"app", "10.%"}, {"etl", "localhost"}}); !reflect.DeepEqual(got, []string{"etl@localhost", "shop@%"}) {
		t.Fatalf("TiDB others = %v", got)
	}
	if got := cassandraOtherRoles([]string{"svc-1-superuser", "app", "cassandra", "backdoor"}, "svc-1-superuser"); !reflect.DeepEqual(got, []string{"backdoor"}) {
		t.Fatalf("Cassandra others = %v", got)
	}
}
