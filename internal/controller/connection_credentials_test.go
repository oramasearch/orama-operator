// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	testCredentialCluster    = "prod-svc-0123456789ab"
	testSuperuserPassword    = "superuser-password-never-delivered"
	testOwnerPassword        = "owner-password-never-delivered"
	testCredentialMountID    = "70000000-0000-4000-8000-000000000007"
	testCredentialSecretName = "connection-" + testConnectionID
	testPencilVerifier       = "SCRAM-SHA-256$4096:W22ZaJ0SNY7soEsUEjb6gQ==$WG5d8oPm3OtcPnkdi4Uo7BkeZkBFzpcXkuLmtbsT4qY=:wfPLwcE6nTWhTAmQ7tl2KeoiWGPlZqQxSrmfPwDl2dU="
)

// fakeRoles is an in-memory Postgres: role name to its verifier and scope.
type fakeRoles struct {
	roles      map[string]fakeRole
	sets       int
	applies    int
	drops      []string
	schemas    map[string]bool
	lastTarget ConnectionPostgresTarget
}

type fakeRole struct {
	verifier string
	scope    oramav1alpha1.ConnectionCredentialScope
}

func newFakeRoles() *fakeRoles {
	return &fakeRoles{roles: map[string]fakeRole{}, schemas: map[string]bool{"public": true}}
}

func (f *fakeRoles) RoleExists(_ context.Context, target ConnectionPostgresTarget, role string) (bool, error) {
	f.lastTarget = target
	_, found := f.roles[role]
	return found, nil
}

func (f *fakeRoles) SetRole(_ context.Context, target ConnectionPostgresTarget, role, verifier string, scope oramav1alpha1.ConnectionCredentialScope) error {
	f.lastTarget = target
	if scope.Schema != "" && !f.schemas[scope.Schema] {
		return fmt.Errorf("%w: %s", errConnectionSchemaMissing, scope.Schema)
	}
	f.sets++
	f.roles[role] = fakeRole{verifier: verifier, scope: scope}
	return nil
}

func (f *fakeRoles) ApplyScope(_ context.Context, target ConnectionPostgresTarget, role string, scope oramav1alpha1.ConnectionCredentialScope) error {
	f.lastTarget = target
	if scope.Schema != "" && !f.schemas[scope.Schema] {
		return fmt.Errorf("%w: %s", errConnectionSchemaMissing, scope.Schema)
	}
	f.applies++
	current := f.roles[role]
	current.scope = scope
	f.roles[role] = current
	return nil
}

func (f *fakeRoles) DropRole(_ context.Context, target ConnectionPostgresTarget, role string) error {
	f.lastTarget = target
	f.drops = append(f.drops, role)
	delete(f.roles, role)
	return nil
}

// fakeWorkspaceSecrets is an in-memory Lake secret store.
type fakeWorkspaceSecrets struct {
	puts []fakeSecretPut
}

type fakeSecretPut struct {
	customerID, workspaceID, name string
	value                         map[string]any
	metadata                      map[string]string
}

func (f *fakeWorkspaceSecrets) PutWorkspaceSecret(
	_ context.Context,
	_ *oramav1alpha1.Lake,
	customerID, workspaceID, name string,
	value map[string]any,
	metadata map[string]string,
) (string, error) {
	f.puts = append(f.puts, fakeSecretPut{customerID, workspaceID, name, value, metadata})
	return lakeSecretReference(testCredentialMountID, name), nil
}

type credentialFixture struct {
	ctx        context.Context
	kube       client.Client
	reconciler *ConnectionReconciler
	roles      *fakeRoles
	secrets    *fakeWorkspaceSecrets
	request    ctrl.Request
}

// newCredentialFixture is an app-to-Postgres-service Connection with a credential scope, the
// service's ready CloudNativePG cluster, its superuser and owner Secrets, and the source app.
func newCredentialFixture(t *testing.T, scope *oramav1alpha1.ConnectionCredentialScope, extra ...client.Object) *credentialFixture {
	t.Helper()
	scheme := newLakeScheme(t)
	lake, sandbox, connection := ownedConnectionObjects(t)
	connection.Spec.SourceKind = oramav1alpha1.ConnectionSourceApp
	connection.Spec.CredentialScope = scope
	app := &oramav1alpha1.LakeApp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "source-app",
			Namespace: lakeNamespace(lake),
			Labels:    map[string]string{lakeAppRowIDLabel: testConnectionSourceID},
		},
		Spec: oramav1alpha1.LakeAppSpec{
			LakeRef: lake.Name,
			Sync: &oramav1alpha1.LakeAppSync{
				ID:                 testConnectionSourceID,
				WorkspaceID:        testWorkspaceID,
				WorkspaceNamespace: connection.Namespace,
			},
		},
	}
	cluster := newCNPGCluster(connection.Namespace, testCredentialCluster)
	cluster.SetLabels(map[string]string{serviceIDLabel: testConnectionTargetID, serviceKindLabel: serviceKindPostgres})
	if err := unstructured.SetNestedField(cluster.Object, int64(1), "status", "readyInstances"); err != nil {
		t.Fatal(err)
	}
	for _, object := range []client.Object{app, cluster} {
		if err := controllerutil.SetControllerReference(lake, object, scheme); err != nil {
			t.Fatal(err)
		}
	}
	superuser := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testCredentialCluster + "-superuser", Namespace: connection.Namespace},
		Data:       map[string][]byte{"username": []byte("postgres"), "password": []byte(testSuperuserPassword)},
	}
	owner := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testCredentialCluster + "-app", Namespace: connection.Namespace},
		Data: map[string][]byte{
			"username": []byte(servicePostgresOwner),
			"password": []byte(testOwnerPassword),
			"uri":      []byte("postgresql://app:" + testOwnerPassword + "@host:5432/app"),
		},
	}
	ca := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testCredentialCluster + "-ca", Namespace: connection.Namespace},
		Data:       map[string][]byte{"ca.crt": testPostgresCAPEM(t)},
	}
	objects := append([]client.Object{lake, sandbox, connection, app, cluster, superuser, owner, ca}, extra...)
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&oramav1alpha1.Connection{}).
		WithObjects(objects...).
		Build()
	roles := newFakeRoles()
	secrets := &fakeWorkspaceSecrets{}
	return &credentialFixture{
		ctx:        context.Background(),
		kube:       kube,
		reconciler: &ConnectionReconciler{Client: kube, Scheme: scheme, Roles: roles, Secrets: secrets},
		roles:      roles,
		secrets:    secrets,
		request:    ctrl.Request{NamespacedName: client.ObjectKeyFromObject(connection)},
	}
}

func (f *credentialFixture) reconcile(t *testing.T) oramav1alpha1.Connection {
	t.Helper()
	if _, err := f.reconciler.Reconcile(f.ctx, f.request); err != nil {
		t.Fatal(err)
	}
	return f.connection(t)
}

func (f *credentialFixture) connection(t *testing.T) oramav1alpha1.Connection {
	t.Helper()
	var got oramav1alpha1.Connection
	if err := f.kube.Get(f.ctx, f.request.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func (f *credentialFixture) update(t *testing.T, mutate func(*oramav1alpha1.Connection)) {
	t.Helper()
	got := f.connection(t)
	mutate(&got)
	if err := f.kube.Update(f.ctx, &got); err != nil {
		t.Fatal(err)
	}
}

func readScope() *oramav1alpha1.ConnectionCredentialScope {
	return &oramav1alpha1.ConnectionCredentialScope{Access: oramav1alpha1.ConnectionAccessRead}
}

func TestReadScopedConnectionIssuesItsOwnRoleAndWorkspaceSecret(t *testing.T) {
	f := newCredentialFixture(t, readScope())
	got := f.reconcile(t)
	role := connectionRoleName(testConnectionID)
	if got.Status.Phase != oramav1alpha1.ConnectionPhaseReady ||
		got.Status.CredentialSecret != lakeSecretReference(testCredentialMountID, testCredentialSecretName) ||
		got.Status.CredentialRole != role || got.Status.CredentialGeneration != 1 {
		t.Fatalf("status = %+v", got.Status)
	}
	if len(role) > 63 || !strings.HasPrefix(role, connectionRolePrefix) || role == servicePostgresOwner {
		t.Fatalf("role name %q", role)
	}
	issued, found := f.roles.roles[role]
	if !found || issued.scope.Access != oramav1alpha1.ConnectionAccessRead || issued.scope.Schema != "" ||
		!strings.HasPrefix(issued.verifier, "SCRAM-SHA-256$4096:") {
		t.Fatalf("role = %+v (found %v)", issued, found)
	}
	// The operator signs in as the service's superuser, which only it reads.
	// ... over a session that verifies the server against the cluster's CA (orama-operator#775).
	if f.roles.lastTarget.User != "postgres" || f.roles.lastTarget.Password != testSuperuserPassword ||
		f.roles.lastTarget.Host != testCredentialCluster+"-rw.workspace.svc" || len(f.roles.lastTarget.CA) == 0 {
		t.Fatalf("admin target = %+v", f.roles.lastTarget)
	}
	if len(f.secrets.puts) != 1 {
		t.Fatalf("workspace secret puts = %d", len(f.secrets.puts))
	}
	put := f.secrets.puts[0]
	if put.customerID != testServiceCustomer || put.workspaceID != testWorkspaceID || put.name != testCredentialSecretName {
		t.Fatalf("put = %+v", put)
	}
	password, _ := put.value["password"].(string)
	if put.value["user"] != role || len(password) != 64 || put.value["database"] != servicePostgresDatabase ||
		!strings.HasPrefix(put.value["uri"].(string), "postgresql://"+role+":"+password+"@"+testCredentialCluster+"-rw.workspace.svc.cluster.local:5432/app") {
		t.Fatalf("credential value = %+v", put.value)
	}
	// Acceptance: the service's own owner credential is never handed to the app.
	for key, value := range put.value {
		text := fmt.Sprint(value)
		if strings.Contains(text, testOwnerPassword) || strings.Contains(text, testSuperuserPassword) {
			t.Fatalf("credential field %s carries a service credential", key)
		}
	}
	if put.value["user"] == servicePostgresOwner || put.value["user"] == "postgres" {
		t.Fatal("credential names the service's own role")
	}
	if issued.verifier == password || strings.Contains(issued.verifier, password) {
		t.Fatal("the plaintext password reached the role statement")
	}
	if put.metadata["generation"] != "1" || put.metadata["connection_id"] != testConnectionID || put.metadata["access"] != "read" {
		t.Fatalf("metadata = %+v", put.metadata)
	}
	// The network path is rendered too: a credential never replaces the policy.
	egressName, _ := connectionPolicyNames(&got)
	var egress networkingv1.NetworkPolicy
	if err := f.kube.Get(f.ctx, client.ObjectKey{Namespace: got.Namespace, Name: egressName}, &egress); err != nil {
		t.Fatal(err)
	}

	// A steady reconcile re-applies the grants and issues nothing new.
	got = f.reconcile(t)
	if f.roles.sets != 1 || len(f.secrets.puts) != 1 || f.roles.applies != 1 || got.Status.Phase != oramav1alpha1.ConnectionPhaseReady {
		t.Fatalf("steady reconcile: sets=%d puts=%d applies=%d status=%+v", f.roles.sets, len(f.secrets.puts), f.roles.applies, got.Status)
	}
}

func TestNewGenerationRotatesTheCredentialUnderTheSameSecret(t *testing.T) {
	f := newCredentialFixture(t, readScope())
	first := f.reconcile(t)
	firstVerifier := f.roles.roles[connectionRoleName(testConnectionID)].verifier
	f.update(t, func(c *oramav1alpha1.Connection) { c.Spec.Generation = 2 })
	got := f.reconcile(t)
	if f.roles.sets != 2 || len(f.secrets.puts) != 2 {
		t.Fatalf("rotation: sets=%d puts=%d", f.roles.sets, len(f.secrets.puts))
	}
	if f.roles.roles[connectionRoleName(testConnectionID)].verifier == firstVerifier {
		t.Fatal("rotation kept the old verifier")
	}
	if f.secrets.puts[0].value["password"] == f.secrets.puts[1].value["password"] {
		t.Fatal("rotation kept the old password")
	}
	if got.Status.CredentialSecret != first.Status.CredentialSecret || got.Status.CredentialGeneration != 2 ||
		got.Status.ObservedGeneration != 2 || f.secrets.puts[1].metadata["generation"] != "2" {
		t.Fatalf("rotated status = %+v", got.Status)
	}
}

func TestRevocationDropsTheRoleInOneReconcile(t *testing.T) {
	f := newCredentialFixture(t, readScope())
	f.reconcile(t)
	f.update(t, func(c *oramav1alpha1.Connection) {
		c.Spec.Desired = oramav1alpha1.ConnectionDesiredAbsent
		c.Spec.Generation = 2
	})
	got := f.reconcile(t)
	role := connectionRoleName(testConnectionID)
	if len(f.roles.drops) != 1 || f.roles.drops[0] != role {
		t.Fatalf("drops = %v", f.roles.drops)
	}
	if _, found := f.roles.roles[role]; found {
		t.Fatal("role survives revocation")
	}
	if got.Status.Phase != oramav1alpha1.ConnectionPhaseDeleted || got.Status.CredentialSecret != "" || got.Status.CredentialRole != "" {
		t.Fatalf("revoked status = %+v", got.Status)
	}
	egressName, ingressName := connectionPolicyNames(&got)
	for _, name := range []string{egressName, ingressName} {
		var policy networkingv1.NetworkPolicy
		if err := f.kube.Get(f.ctx, client.ObjectKey{Namespace: got.Namespace, Name: name}, &policy); client.IgnoreNotFound(err) != nil || err == nil {
			t.Fatalf("policy %s remains after revocation: %v", name, err)
		}
	}
}

func TestDeletingTheConnectionDropsTheRole(t *testing.T) {
	f := newCredentialFixture(t, readScope())
	f.reconcile(t)
	got := f.connection(t)
	if err := f.kube.Delete(f.ctx, &got); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reconciler.Reconcile(f.ctx, f.request); err != nil {
		t.Fatal(err)
	}
	if len(f.roles.drops) != 1 {
		t.Fatalf("drops = %v", f.roles.drops)
	}
	var gone oramav1alpha1.Connection
	if err := f.kube.Get(f.ctx, f.request.NamespacedName, &gone); client.IgnoreNotFound(err) != nil || err == nil {
		t.Fatalf("Connection remains after its finalizer: %v", err)
	}
}

func TestRemovingTheScopeDropsTheRoleAndKeepsThePath(t *testing.T) {
	f := newCredentialFixture(t, readScope())
	f.reconcile(t)
	f.update(t, func(c *oramav1alpha1.Connection) {
		c.Spec.CredentialScope = nil
		c.Spec.Generation = 2
	})
	got := f.reconcile(t)
	if len(f.roles.drops) != 1 || got.Status.Phase != oramav1alpha1.ConnectionPhaseReady ||
		got.Status.CredentialSecret != "" || got.Status.CredentialRole != "" {
		t.Fatalf("drops=%v status=%+v", f.roles.drops, got.Status)
	}
}

func TestAVanishedRoleIsIssuedAgain(t *testing.T) {
	f := newCredentialFixture(t, readScope())
	f.reconcile(t)
	// The cluster was recreated: the role is gone although the status says it was issued.
	delete(f.roles.roles, connectionRoleName(testConnectionID))
	f.reconcile(t)
	if f.roles.sets != 2 || len(f.secrets.puts) != 2 {
		t.Fatalf("reissue: sets=%d puts=%d", f.roles.sets, len(f.secrets.puts))
	}
}

func TestSchemaScopeWaitsForItsSchema(t *testing.T) {
	scope := &oramav1alpha1.ConnectionCredentialScope{Access: oramav1alpha1.ConnectionAccessWrite, Schema: "analytics"}
	f := newCredentialFixture(t, scope)
	got := f.reconcile(t)
	if got.Status.Phase != oramav1alpha1.ConnectionPhasePending || !strings.Contains(got.Status.Message, "analytics") ||
		len(f.secrets.puts) != 0 {
		t.Fatalf("status = %+v puts=%d", got.Status, len(f.secrets.puts))
	}
	f.roles.schemas["analytics"] = true
	got = f.reconcile(t)
	issued := f.roles.roles[connectionRoleName(testConnectionID)]
	if got.Status.Phase != oramav1alpha1.ConnectionPhaseReady || issued.scope != *scope {
		t.Fatalf("status = %+v scope = %+v", got.Status, issued.scope)
	}
}

func TestCredentialOnANonPostgresServiceFails(t *testing.T) {
	f := newCredentialFixture(t, readScope(), &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kafka-target",
			Namespace: "workspace",
			Labels:    map[string]string{serviceIDLabel: testConnectionTargetID, serviceKindLabel: "kafka"},
		},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "kafka", Port: 9092}}},
	})
	// Re-point the target at the Kafka service: no Postgres cluster carries its id.
	cluster := newCNPGCluster("workspace", testCredentialCluster)
	if err := f.kube.Get(f.ctx, client.ObjectKeyFromObject(cluster), cluster); err != nil {
		t.Fatal(err)
	}
	cluster.SetLabels(map[string]string{serviceIDLabel: testConnectionSourceID})
	if err := f.kube.Update(f.ctx, cluster); err != nil {
		t.Fatal(err)
	}
	got := f.reconcile(t)
	if got.Status.Phase != oramav1alpha1.ConnectionPhaseFailed || !strings.Contains(got.Status.Message, "kafka") {
		t.Fatalf("status = %+v", got.Status)
	}
	if len(f.roles.roles) != 0 || len(f.secrets.puts) != 0 {
		t.Fatal("a credential was issued for a non-Postgres service")
	}
	// Revocation has nothing to drop and does not block the finalizer.
	f.update(t, func(c *oramav1alpha1.Connection) {
		c.Spec.Desired = oramav1alpha1.ConnectionDesiredAbsent
		c.Spec.Generation = 2
	})
	if got = f.reconcile(t); got.Status.Phase != oramav1alpha1.ConnectionPhaseDeleted {
		t.Fatalf("status = %+v", got.Status)
	}
}

// orama-operator#775: without the cluster's CA the operator does not sign in at all.
func TestCredentialWaitsForThePostgresCA(t *testing.T) {
	f := newCredentialFixture(t, readScope())
	if err := f.kube.Delete(f.ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: testCredentialCluster + "-ca", Namespace: "workspace",
	}}); err != nil {
		t.Fatal(err)
	}
	result, err := f.reconciler.Reconcile(f.ctx, f.request)
	if err != nil || result.RequeueAfter != connectionCredentialRequeue {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if f.roles.lastTarget.Password != "" {
		t.Fatal("the operator signed in without the CA")
	}
}

func TestCredentialWaitsForTheSuperuserSecret(t *testing.T) {
	f := newCredentialFixture(t, readScope())
	if err := f.kube.Delete(f.ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: testCredentialCluster + "-superuser", Namespace: "workspace",
	}}); err != nil {
		t.Fatal(err)
	}
	result, err := f.reconciler.Reconcile(f.ctx, f.request)
	if err != nil || result.RequeueAfter != connectionCredentialRequeue {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if got := f.connection(t); got.Status.Phase != oramav1alpha1.ConnectionPhasePending {
		t.Fatalf("status = %+v", got.Status)
	}
}

func TestScramVerifierMatchesAnIndependentImplementation(t *testing.T) {
	// Computed with Python's hashlib/hmac (RFC 5802 StoredKey and ServerKey) for password
	// "pencil", salt "W22ZaJ0SNY7soEsUEjb6gQ==" and 4096 iterations.
	salt := []byte{0x5b, 0x6d, 0x99, 0x68, 0x9d, 0x12, 0x35, 0x8e, 0xec, 0xa0, 0x4b, 0x14, 0x12, 0x36, 0xfa, 0x81}
	got := scramSHA256VerifierWithSalt("pencil", salt, 4096)
	if got != testPencilVerifier {
		t.Fatalf("verifier = %s", got)
	}
}
