/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// What a stack does for the migrations that target it (operator#653): its sandbox admits their
// pods, its control DB's migration role signs in only while one exists, and its Prod 1 egress
// exists only while one does.

func migrationSandbox(t *testing.T, oc *oramav1alpha1.OramaCloudV1, objs ...client.Object) *oramav1alpha1.NetworkSandbox {
	t.Helper()
	ctx := context.Background()
	scheme := ocv1TestScheme(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(append(objs, oc), sandboxDiscoveryObjects()...)...).Build()
	r := &OramaCloudV1Reconciler{Client: kube, Scheme: scheme, APIReader: kube, OperatorNamespace: "orama-system"}
	if err := r.reconcileNetworkSandbox(ctx, oc); err != nil {
		t.Fatal(err)
	}
	sandbox := &oramav1alpha1.NetworkSandbox{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: networkSandboxName}, sandbox); err != nil {
		t.Fatal(err)
	}
	assertJustifiedNarrowAllowances(t, sandbox)
	return sandbox
}

func migrationSelects(t *testing.T, sel *metav1.LabelSelector, l map[string]string) bool {
	t.Helper()
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		t.Fatal(err)
	}
	return s.Matches(labels.Set(l))
}

func TestStackSandboxAdmitsMigrationPods(t *testing.T) {
	oc := migrationTestStack(nil)
	m := migrationTestResource(nil)
	pod := migrationLabels(oc, m)
	sandbox := migrationSandbox(t, oc)

	// The pod is inside the sandbox, so default-deny and the DNS allowance apply to it.
	if !migrationSelects(t, &sandbox.Spec.PodSelector, pod) {
		t.Fatal("the migration pod is outside the stack's sandbox")
	}
	if dns := allowanceByName(t, sandbox, "allow-dns-egress"); !migrationSelects(t, &dns.Spec.PodSelector, pod) {
		t.Fatal("allow-dns-egress does not select the migration pod")
	}

	egress := allowanceByName(t, sandbox, "allow-migration-egress")
	if !migrationSelects(t, &egress.Spec.PodSelector, pod) || migrationSelects(t, &egress.Spec.PodSelector, labelsFor(oc, "dashboard")) {
		t.Fatal("allow-migration-egress selects the wrong pods")
	}
	type path struct {
		component string
		port      int32
	}
	var got []path
	for _, rule := range egress.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil || peer.NamespaceSelector != nil {
				t.Fatalf("allow-migration-egress leaves the namespace: %+v", peer)
			}
			target := "controldb"
			switch {
			case migrationSelects(t, peer.PodSelector, labelsFor(oc, "writer")):
				target = "writer"
			case migrationSelects(t, peer.PodSelector, labelsFor(oc, "reader")):
				target = "reader"
			case !migrationSelects(t, peer.PodSelector, map[string]string{"cnpg.io/cluster": oc.Name + "-controldb"}):
				t.Fatalf("allow-migration-egress reaches %+v", peer.PodSelector)
			}
			for _, p := range rule.Ports {
				got = append(got, path{target, p.Port.IntVal})
			}
		}
	}
	if want := []path{{"controldb", 5432}, {"writer", 8080}, {"reader", 80}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("migration egress = %v, want %v", got, want)
	}

	for name, target := range map[string]map[string]string{
		"allow-controldb-migration-ingress": {"cnpg.io/cluster": oc.Name + "-controldb"},
		"allow-writer-migration-ingress":    labelsFor(oc, "writer"),
		"allow-reader-migration-ingress":    labelsFor(oc, "reader"),
	} {
		ingress := allowanceByName(t, sandbox, name)
		if !migrationSelects(t, &ingress.Spec.PodSelector, target) || len(ingress.Spec.Ingress) != 1 ||
			!migrationSelects(t, ingress.Spec.Ingress[0].From[0].PodSelector, pod) {
			t.Errorf("%s does not admit the migration pod: %+v", name, ingress.Spec)
		}
	}

	// No migration targets the stack: no path to Prod 1, or anywhere outside the cluster.
	for _, a := range sandbox.Spec.Policies {
		if a.Name == migrationProd1PolicyName {
			t.Fatal("Prod 1 egress rendered with no migration")
		}
		if len(a.Spec.Egress) > 0 && migrationSelects(t, &a.Spec.PodSelector, pod) {
			for _, rule := range a.Spec.Egress {
				for _, peer := range rule.To {
					if peer.IPBlock != nil {
						t.Errorf("%s gives the migration pod an outside address with no migration", a.Name)
					}
				}
			}
		}
	}
}

func TestStackSandboxOpensProd1OnlyForItsMigrations(t *testing.T) {
	oc := migrationTestStack(nil)
	other := migrationTestResource(func(m *oramav1alpha1.OramaCloudV1Migration) {
		m.Name, m.Spec.TargetRef = "other-team", "v1-other"
		m.Spec.Source.DatabaseHost = "other.psdb.cloud"
	})
	sandbox := migrationSandbox(t, oc, other)
	for _, a := range sandbox.Spec.Policies {
		if a.Name == migrationProd1PolicyName {
			t.Fatal("another stack's migration opened this stack's Prod 1 egress")
		}
	}

	second := migrationTestResource(func(m *oramav1alpha1.OramaCloudV1Migration) {
		m.Name = "team-2"
		m.Spec.Source.ReaderURL = "https://reader-2.prod1.example.com"
	})
	sandbox = migrationSandbox(t, oc, migrationTestResource(nil), second, other)
	prod1 := allowanceByName(t, sandbox, migrationProd1PolicyName)
	if !migrationSelects(t, &prod1.Spec.PodSelector, migrationLabels(oc, second)) || migrationSelects(t, &prod1.Spec.PodSelector, labelsFor(oc, "writer")) {
		t.Fatal("Prod 1 egress selects the wrong pods")
	}
	if len(prod1.Spec.Egress) != 1 {
		t.Fatalf("Prod 1 egress rules = %+v", prod1.Spec.Egress)
	}
	rule := prod1.Spec.Egress[0]
	var ports []int32
	for _, p := range rule.Ports {
		if *p.Protocol != corev1.ProtocolTCP {
			t.Fatalf("port %+v", p)
		}
		ports = append(ports, p.Port.IntVal)
	}
	if !reflect.DeepEqual(ports, []int32{443, 5432}) {
		t.Fatalf("Prod 1 ports = %v, want 443 and 5432", ports)
	}
	for _, peer := range rule.To {
		if peer.IPBlock == nil || peer.PodSelector != nil || len(peer.IPBlock.Except) == 0 {
			t.Fatalf("Prod 1 peer %+v is not a public range with the cluster's ranges excluded", peer)
		}
	}
	// The hosts it is for: exactly each database on 5432, the engine origins and the region's SSM on 443.
	hosts := "aws.connect.psdb.cloud:5432, reader-2.prod1.example.com:443, reader.prod1.example.com:443, ssm.us-east-2.amazonaws.com:443, writer.prod1.example.com:443"
	if !strings.Contains(prod1.Comment, "at "+hosts+" (operator#653)") || strings.Contains(prod1.Comment, "other.psdb.cloud") {
		t.Fatalf("Prod 1 egress comment = %q", prod1.Comment)
	}
	if got := migrationProd1Hosts([]oramav1alpha1.OramaCloudV1Migration{*migrationTestResource(nil)}); !reflect.DeepEqual(got, []string{
		"aws.connect.psdb.cloud:5432", "reader.prod1.example.com:443", "ssm.us-east-2.amazonaws.com:443", "writer.prod1.example.com:443",
	}) {
		t.Fatalf("hosts = %v", got)
	}
}

func TestStackReaderPortFollowsEnvoy(t *testing.T) {
	oc := migrationTestStack(func(oc *oramav1alpha1.OramaCloudV1) { oc.Spec.Components.Engine.Envoy = false })
	sandbox := migrationSandbox(t, oc)
	ingress := allowanceByName(t, sandbox, "allow-reader-migration-ingress")
	if p := ingress.Spec.Ingress[0].Ports[0].Port; *p != intstr.FromInt32(oramacorePort) {
		t.Fatalf("reader port without Envoy = %v", p)
	}
}

// The migration role exists on every stack; only login follows the migrations targeting it.
func TestControlDBMigrationRoleLoginFollowsMigrations(t *testing.T) {
	ctx := context.Background()
	oc := newControlDBStack()
	other := migrationTestResource(func(m *oramav1alpha1.OramaCloudV1Migration) {
		m.Namespace, m.Spec.TargetRef = oc.Namespace, "another-stack"
	})
	r, kube := newControlDBReconciler(t, oc, other)
	role := func() map[string]interface{} {
		t.Helper()
		if err := r.reconcileControlDB(ctx, oc); err != nil {
			t.Fatal(err)
		}
		cluster := &unstructured.Unstructured{}
		cluster.SetGroupVersionKind(cnpgClusterGVK)
		if err := kube.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-controldb"}, cluster); err != nil {
			t.Fatal(err)
		}
		roles, _, err := unstructured.NestedSlice(cluster.Object, "spec", "managed", "roles")
		if err != nil || len(roles) != 1 {
			t.Fatalf("managed roles = %v (%v)", roles, err)
		}
		return roles[0].(map[string]interface{})
	}

	got := role()
	want := map[string]interface{}{
		"name": "migration", "comment": "OramaCloudV1Migration copies from Prod 1 (operator#653)", "ensure": "present",
		"login": false, "superuser": false, "createdb": false, "createrole": false, "replication": false, "bypassrls": false,
		"inherit": true, "connectionLimit": int64(4), "passwordSecret": map[string]interface{}{"name": "v1-controldb-migration"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("role with no migration = %v, want %v", got, want)
	}

	mine := migrationTestResource(func(m *oramav1alpha1.OramaCloudV1Migration) {
		m.Name, m.Namespace, m.Spec.TargetRef = "mine", oc.Namespace, oc.Name
	})
	if err := kube.Create(ctx, mine); err != nil {
		t.Fatal(err)
	}
	if got := role(); got["login"] != true {
		t.Fatalf("login with a migration = %v", got["login"])
	}
	if err := kube.Delete(ctx, mine); err != nil {
		t.Fatal(err)
	}
	if got := role(); got["login"] != false {
		t.Fatalf("login once the migration is gone = %v", got["login"])
	}

	// The password Secret: generated once, never changed, the tool's DSN in uri.
	sec := &corev1.Secret{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: "v1-controldb-migration"}, sec); err != nil {
		t.Fatal(err)
	}
	pass := string(sec.Data["password"])
	if sec.Type != corev1.SecretTypeBasicAuth || string(sec.Data["username"]) != "migration" || len(pass) != 48 ||
		string(sec.Data["uri"]) != "postgresql://migration:"+pass+"@v1-controldb-rw.orama-system:5432/oramacore" ||
		sec.Labels["cnpg.io/reload"] != "true" || !metav1.IsControlledBy(sec, oc) {
		t.Fatalf("migration role Secret = %v %v %v", sec.Type, sec.Labels, sec.OwnerReferences)
	}
	role()
	again := &corev1.Secret{}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(sec), again); err != nil || string(again.Data["password"]) != pass {
		t.Fatalf("the password changed: %v", err)
	}
}

// The grants are the last control DB schema: read and write rows and use sequences in public, never
// more, and again whenever a schema that creates tables changes.
func TestControlDBMigrationGrants(t *testing.T) {
	oc := newControlDBStack()
	schemas, err := controlDBSchemas(oc)
	if err != nil {
		t.Fatal(err)
	}
	last := schemas[len(schemas)-1]
	if last.suffix != "-controldb-migration-grants" || !last.everyVersion || last.key != "migration-grants.sql" {
		t.Fatalf("last schema = %+v", last)
	}
	for _, want := range []string{
		"GRANT USAGE ON SCHEMA public TO migration;",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO migration;",
		"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO migration;",
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO migration;",
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO migration;",
		"controldb-schema " + schemaVersion(dashboardSchemaSQL),
		"controldb-schema-0008 " + schemaVersion(mustSQL(controlDBSchema0008SQL())),
	} {
		if !strings.Contains(last.sql, want) {
			t.Errorf("grants lack %q:\n%s", want, last.sql)
		}
	}
	for _, never := range []string{"TRUNCATE", "CREATE", "SUPERUSER", "ALL PRIVILEGES", "pg_write_all_data", "reference-data"} {
		if strings.Contains(last.sql, never) {
			t.Errorf("grants contain %q", never)
		}
	}
	// Stripe prices change the reference rows, not the grants.
	oc.Spec.Components.ControlDB.StripePrices.Plans = []oramav1alpha1.StripePlanPrice{{ID: "0d735744-e58e-4438-b618-97523e407cdc", Monthly: "price_test"}}
	again, err := controlDBSchemas(oc)
	if err != nil {
		t.Fatal(err)
	}
	if again[len(again)-1].sql != last.sql {
		t.Fatal("a Stripe price change changed the grants")
	}
}

// requestsForMigration maps a migration to the stack it targets.
func TestMigrationEnqueuesItsTargetStack(t *testing.T) {
	r := &OramaCloudV1Reconciler{}
	got := r.requestsForMigration(context.Background(), migrationTestResource(nil))
	if len(got) != 1 || got[0].Namespace != "v1-dev" || got[0].Name != "v1-dev" {
		t.Fatalf("requests = %v", got)
	}
	if got := r.requestsForMigration(context.Background(), &networkingv1.NetworkPolicy{}); got != nil {
		t.Fatalf("a non-migration mapped to %v", got)
	}
}
