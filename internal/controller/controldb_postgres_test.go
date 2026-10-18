/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// referenceRowJSON returns the JSON of every row dashboard-reference-rows.sql stages into table, as
// one array in file order.
func referenceRowJSON(t *testing.T, table string) string {
	t.Helper()
	var rows []string
	for _, m := range regexp.MustCompile(`(?m)^INSERT INTO reference_`+table+` SELECT \* FROM json_populate_record\(NULL::reference_`+table+`, '(.*)'\);$`).FindAllStringSubmatch(dashboardReferenceRowsSQL, -1) {
		rows = append(rows, strings.ReplaceAll(m[1], "''", "'"))
	}
	if len(rows) == 0 {
		t.Fatalf("dashboard-reference-rows.sql has no %s rows", table)
	}
	return "[" + strings.Join(rows, ",") + "]"
}

const (
	fixtureUserID = "11111111-1111-4111-8111-111111111111"
	fixtureTeamID = "44444444-4444-4444-8444-444444444444"
)

// TestControlDBSchemasAgainstPostgres applies the control DB's schemas to real PostgreSQL databases
// through the operator's own reconcilers: the OramaCloudV1 reconciler renders the ConfigMap and the
// ordered PostgresSchemas, the PostgresSchema reconciler renders each migration Job, and the Job's
// script runs natively against a fresh database (there is no kubelet). The reference rows are the
// committed export from Prod 1. Opt in with
// CONTROLDB_SCHEMA_TEST_POSTGRES_URI, an admin connection to a disposable server; each case creates
// and drops its own database.
func TestControlDBSchemasAgainstPostgres(t *testing.T) {
	admin := os.Getenv("CONTROLDB_SCHEMA_TEST_POSTGRES_URI")
	if admin == "" {
		t.Skip("CONTROLDB_SCHEMA_TEST_POSTGRES_URI is required for the control DB schema proof")
	}
	t.Run("empty database", func(t *testing.T) {
		p := newControlDBProof(t, admin)
		p.converge()
		p.verifySchema()
		p.verifyReferenceRows()
		p.verifyTeamCreation()
		// Reconcile again, then run 0008's Job once more: the guard skips it and nothing changes.
		p.reconcileStack()
		if out := p.runSchemaScript("-controldb-schema-0008"); !strings.Contains(out, "schema already applied (team_migrations present)") {
			t.Fatalf("complete 0008 was not skipped: %s", out)
		}
		p.verifySchema()
	})

	t.Run("0000-0007 with a team", func(t *testing.T) {
		p := newControlDBProof(t, admin)
		p.sql(dashboardSchemaSQL)
		p.seedTeam()
		// 0008 alone cannot validate the existing team's default plan: the whole file rolls back.
		if out, err := p.trySQLFile(dashboardSchema0008SQL); err == nil || !strings.Contains(out, "teams_subscription_plan_id_subscription_plans_id_fk") {
			t.Fatalf("0008 without reference rows: err=%v\n%s", err, out)
		}
		if got := p.sql("SELECT to_regclass('public.team_migrations') IS NULL AND to_regclass('public.subscription_plans') IS NULL"); got != "t" {
			t.Fatal("failed 0008 left objects behind")
		}
		t.Log("0008 alone rolled back on the missing default plan")

		p.converge()
		p.verifySchema()
		if got := p.sql("SELECT name || ' ' || subscription_plan_id FROM teams WHERE id = '" + fixtureTeamID + "'"); got != "Existing fixture team 0d735744-e58e-4438-b618-97523e407cdc" {
			t.Fatalf("existing team after 0008 = %q", got)
		}
		p.verifyTeamCreation()
	})

	t.Run("missing reference rows fail, then recover", func(t *testing.T) {
		p := newControlDBProof(t, admin)
		p.sql(dashboardSchemaSQL)
		p.seedTeam()
		rows := dashboardReferenceRowsSQL
		t.Cleanup(func() { dashboardReferenceRowsSQL = rows })
		dashboardReferenceRowsSQL = "-- no rows\n"
		p.reconcileStack()
		p.applySchema("-controldb-schema", "Applied")
		p.reconcileStack()
		p.applySchema("-controldb-schema-0008", "Failed")
		if got := p.sql("SELECT to_regclass('public.team_migrations') IS NULL"); got != "t" {
			t.Fatal("failed 0008 left team_migrations behind")
		}
		// The rows arrive with an operator change: 0008's version moves, the PostgresSchema
		// controller replaces the failed Job, and the new one applies.
		dashboardReferenceRowsSQL = rows
		p.converge()
		p.verifySchema()
		p.verifyTeamCreation()
	})

	t.Run("compatible partial 0008 without its guard", func(t *testing.T) {
		p := newControlDBProof(t, admin)
		p.converge()
		p.sql("DROP TABLE team_migrations")
		if out := p.runSchemaScript("-controldb-schema-0008"); !strings.Contains(out, "applying schema") {
			t.Fatalf("partial 0008 was not re-applied: %s", out)
		}
		p.verifySchema()
	})

	t.Run("stack Stripe prices", func(t *testing.T) {
		p := newControlDBProof(t, admin)
		p.converge()
		if got := p.sql("SELECT count(*) FROM stripe_addon_prices"); got != "0" {
			t.Fatalf("add-on prices without a stack price = %s", got)
		}
		addon := p.sql("SELECT (" + deploymentSQLQuote(referenceRowJSON(t, "stripe_addon_prices")) + "::jsonb -> 0) ->> 'id'")
		p.stack.Spec.Components.ControlDB.StripePrices = oramav1alpha1.ControlDBStripePrices{
			Plans:  []oramav1alpha1.StripePlanPrice{{ID: "0d735744-e58e-4438-b618-97523e407cdc", Monthly: "price_test_monthly"}},
			Addons: []oramav1alpha1.StripeAddonPrice{{ID: addon, PriceID: "price_test_addon"}},
		}
		p.converge()
		p.verifyReferenceRows()
		if got := p.sql("SELECT stripe_price_id_monthly || ' ' || (stripe_price_id_yearly IS NULL) FROM subscription_plans WHERE id = '0d735744-e58e-4438-b618-97523e407cdc'"); got != "price_test_monthly true" {
			t.Fatalf("plan Stripe prices = %q", got)
		}
		if got := p.sql("SELECT stripe_price_id FROM stripe_addon_prices WHERE id = '" + addon + "'"); got != "price_test_addon" {
			t.Fatalf("add-on Stripe price = %q", got)
		}
		p.stack.Spec.Components.ControlDB.StripePrices.Plans = []oramav1alpha1.StripePlanPrice{{ID: "55555555-5555-4555-8555-555555555555", Monthly: "price_unknown"}}
		p.reconcileStack()
		p.applySchema("-controldb-reference-data", "Failed")
		if got := p.sql("SELECT stripe_price_id_monthly FROM subscription_plans WHERE id = '0d735744-e58e-4438-b618-97523e407cdc'"); got != "price_test_monthly" {
			t.Fatalf("a failed seed changed a plan: %q", got)
		}
		t.Log("an unknown plan ID failed the seed and changed nothing")
	})
}

type controlDBProof struct {
	t       *testing.T
	ctx     context.Context
	uri     string
	stack   *oramav1alpha1.OramaCloudV1
	kube    client.Client
	oc      *OramaCloudV1Reconciler
	schemas *PostgresSchemaReconciler
}

func newControlDBProof(t *testing.T, admin string) *controlDBProof {
	t.Helper()
	name := fmt.Sprintf("controldb_%d", time.Now().UnixNano())
	runSchemaCommand(t, "psql", admin, "-X", "-qAt", "-v", "ON_ERROR_STOP=1", "-c", "CREATE DATABASE "+name)
	t.Cleanup(func() {
		runSchemaCommand(t, "psql", admin, "-X", "-qAt", "-v", "ON_ERROR_STOP=1", "-c", "DROP DATABASE "+name)
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	stack := newControlDBStack()
	r, kube := newControlDBReconciler(t, stack, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: stack.Name + "-controldb-app", Namespace: stack.Namespace},
		Data:       map[string][]byte{"uri": []byte(u.String())},
	})
	return &controlDBProof{t: t, ctx: context.Background(), uri: u.String(), stack: stack, kube: kube, oc: r,
		schemas: &PostgresSchemaReconciler{Client: kube, Scheme: r.Scheme}}
}

func (p *controlDBProof) sql(query string) string {
	p.t.Helper()
	return runSchemaCommand(p.t, "psql", p.uri, "-X", "-qAt", "-v", "ON_ERROR_STOP=1", "-c", query)
}

// sqlScript runs several statements from a file and returns every result. psql before 15 prints
// only the last result of a multi-statement -c, so a script whose answer is not its last statement
// goes through -f.
func (p *controlDBProof) sqlScript(script string) string {
	p.t.Helper()
	path := filepath.Join(p.t.TempDir(), "script.sql")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		p.t.Fatal(err)
	}
	return runSchemaCommand(p.t, "psql", p.uri, "-X", "-qAt", "-v", "ON_ERROR_STOP=1", "-f", path)
}

// trySQLFile applies a file as the migration Job would, in one transaction, returning its error.
func (p *controlDBProof) trySQLFile(sql string) (string, error) {
	p.t.Helper()
	path := filepath.Join(p.t.TempDir(), "fixture.sql")
	if err := os.WriteFile(path, []byte(sql), 0o600); err != nil {
		p.t.Fatal(err)
	}
	out, err := exec.CommandContext(p.ctx, "psql", p.uri, "-X", "-q", "-v", "ON_ERROR_STOP=1", "--single-transaction", "-f", path).CombinedOutput()
	return string(out), err
}

func (p *controlDBProof) seedTeam() {
	p.t.Helper()
	p.sql(`INSERT INTO users (id, first_name, email, firebase_id) VALUES ('` + fixtureUserID + `', 'Existing fixture', 'existing-fixture@example.invalid', 'existing-fixture-378');
INSERT INTO teams (id, name, created_by) VALUES ('` + fixtureTeamID + `', 'Existing fixture team', '` + fixtureUserID + `');`)
}

func (p *controlDBProof) reconcileStack() {
	p.t.Helper()
	if err := p.oc.reconcileControlDBMigrations(p.ctx, p.stack); err != nil {
		p.t.Fatal(err)
	}
}

func (p *controlDBProof) schema(suffix string) *oramav1alpha1.PostgresSchema {
	p.t.Helper()
	ps, ok := getSchema(p.t, p.kube, p.stack, suffix)
	if !ok {
		p.t.Fatalf("%s not created", p.stack.Name+suffix)
	}
	return ps
}

// reconcileSchema runs the PostgresSchema controller once, finishing the foreground deletion of an
// obsolete Job itself because the fake client has no garbage collector.
func (p *controlDBProof) reconcileSchema(suffix string) *oramav1alpha1.PostgresSchema {
	p.t.Helper()
	key := client.ObjectKeyFromObject(p.schema(suffix))
	for range 3 {
		if _, err := p.schemas.Reconcile(p.ctx, ctrl.Request{NamespacedName: key}); err != nil {
			p.t.Fatal(err)
		}
		var job batchv1.Job
		err := p.kube.Get(p.ctx, client.ObjectKey{Namespace: key.Namespace, Name: key.Name + "-migrate"}, &job)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			p.t.Fatal(err)
		}
		if job.DeletionTimestamp.IsZero() {
			break
		}
		job.Finalizers = nil
		if err := p.kube.Update(p.ctx, &job); err != nil {
			p.t.Fatal(err)
		}
	}
	return p.schema(suffix)
}

// runSchemaScript runs the current migration Job's script natively against the proof database,
// reading the SQL from the stack's ConfigMap.
func (p *controlDBProof) runSchemaScript(suffix string) string {
	out, err := p.tryJob(suffix)
	if err != nil {
		p.t.Fatalf("%s Job failed: %v\n%s", p.stack.Name+suffix, err, out)
	}
	return out
}

func (p *controlDBProof) tryJob(suffix string) (string, error) {
	p.t.Helper()
	ps := p.schema(suffix)
	var job batchv1.Job
	if err := p.kube.Get(p.ctx, client.ObjectKey{Namespace: ps.Namespace, Name: ps.Name + "-migrate"}, &job); err != nil {
		p.t.Fatal(err)
	}
	var cm corev1.ConfigMap
	if err := p.kube.Get(p.ctx, client.ObjectKey{Namespace: ps.Namespace, Name: ps.Spec.SchemaConfigMapRef.Name}, &cm); err != nil {
		p.t.Fatal(err)
	}
	path := filepath.Join(p.t.TempDir(), ps.Spec.SchemaConfigMapRef.Key)
	if err := os.WriteFile(path, []byte(cm.Data[ps.Spec.SchemaConfigMapRef.Key]), 0o600); err != nil {
		p.t.Fatal(err)
	}
	command := job.Spec.Template.Spec.Containers[0].Command
	script := strings.ReplaceAll(command[2], "/migrations/"+ps.Spec.SchemaConfigMapRef.Key, "'"+path+"'")
	cmd := exec.CommandContext(p.ctx, command[0], command[1], script)
	cmd.Env = append(os.Environ(), "DATABASE_URL="+p.uri)
	out, err := cmd.CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "schema already") || strings.HasPrefix(line, "applying schema") || strings.Contains(line, "ERROR") {
			p.t.Logf("%s: %s", ps.Name, line)
		}
	}
	return string(out), err
}

// applySchema drives one PostgresSchema through its Job and checks the phase it ends in.
func (p *controlDBProof) applySchema(suffix, want string) {
	p.t.Helper()
	ps := p.reconcileSchema(suffix)
	if ps.Status.Phase != "Applying" {
		p.t.Fatalf("%s phase = %s before its Job ran", ps.Name, ps.Status.Phase)
	}
	out, err := p.tryJob(suffix)
	var job batchv1.Job
	if err := p.kube.Get(p.ctx, client.ObjectKey{Namespace: ps.Namespace, Name: ps.Name + "-migrate"}, &job); err != nil {
		p.t.Fatal(err)
	}
	if err == nil {
		job.Status.Succeeded = 1
	} else {
		job.Status.Failed = 1
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	}
	if err := p.kube.Status().Update(p.ctx, &job); err != nil {
		p.t.Fatal(err)
	}
	ps = p.reconcileSchema(suffix)
	if ps.Status.Phase != want {
		p.t.Fatalf("%s phase = %s, want %s\n%s", ps.Name, ps.Status.Phase, want, out)
	}
	p.t.Logf("%s: %s %s", ps.Name, ps.Status.Phase, ps.Status.AppliedVersion)
}

// converge reconciles the stack and applies each outstanding schema in order until every one is
// current, as the controllers would.
func (p *controlDBProof) converge() {
	p.t.Helper()
	schemas, err := controlDBSchemas(p.stack)
	if err != nil {
		p.t.Fatal(err)
	}
	for _, s := range schemas {
		p.reconcileStack()
		if ps := p.reconcileSchema(s.suffix); !postgresSchemaCurrent(ps) {
			p.applySchema(s.suffix, "Applied")
		}
	}
	p.reconcileStack()
	if c := schemaComponent(p.t, p.oc, p.stack); !c.Ready {
		p.t.Fatalf("schemas did not converge: %+v", c)
	}
}

func (p *controlDBProof) verifySchema() {
	p.t.Helper()
	checks := map[string]string{
		"tables": `SELECT string_agg(name, ',' ORDER BY name) FROM unnest(ARRAY['limits_errors', 'llms', 'pinning_rules_map',
  'subscription_plans', 'stripe_addon_prices', 'subscription_items', 'team_migrations']) AS t(name) WHERE to_regclass('public.' || name) IS NOT NULL`,
		"enums": `SELECT string_agg(t.typname || '=' || (SELECT string_agg(enumlabel, '|' ORDER BY enumsortorder) FROM pg_enum WHERE enumtypid = t.oid), ',' ORDER BY t.typname)
FROM pg_type t WHERE t.typname IN ('error_type', 'llm_group')`,
		"team columns": `SELECT string_agg(column_name || ' ' || data_type || ' ' || is_nullable || ' ' || coalesce(column_default, '-'), ',' ORDER BY column_name)
FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'teams'
  AND column_name IN ('subscription_plan_id', 'stripe_customer_id', 'stripe_subscription_id', 'has_payment_method')`,
		"constraints": `SELECT string_agg(conname || ' ' || convalidated || ' ' || pg_get_constraintdef(oid), ',' ORDER BY conname) FROM pg_constraint
WHERE conname IN ('teams_subscription_plan_id_subscription_plans_id_fk', 'teams_stripe_customer_id_unique', 'teams_stripe_subscription_id_unique',
  'stripe_addon_prices_plan_id_subscription_plans_id_fk', 'team_migrations_state_check', 'team_migrations_team_id_teams_id_fk',
  'subscription_items_team_id_teams_id_fk', 'stripe_addon_prices_stripe_price_id_unique', 'limits_errors_project_id_projects_id_fk')`,
		"default plan": `SELECT EXISTS (SELECT 1 FROM subscription_plans WHERE id = '0d735744-e58e-4438-b618-97523e407cdc')`,
	}
	want := map[string]string{
		"tables": "limits_errors,llms,pinning_rules_map,stripe_addon_prices,subscription_items,subscription_plans,team_migrations",
		"enums":  "error_type=documents_limit|ai_sessions_limit,llm_group=base|pro|premium",
		"team columns": "has_payment_method boolean NO false," +
			"stripe_customer_id character varying YES -," +
			"stripe_subscription_id character varying YES -," +
			"subscription_plan_id uuid NO '0d735744-e58e-4438-b618-97523e407cdc'::uuid",
		"constraints": "limits_errors_project_id_projects_id_fk true FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE," +
			"stripe_addon_prices_plan_id_subscription_plans_id_fk true FOREIGN KEY (plan_id) REFERENCES subscription_plans(id) ON DELETE CASCADE," +
			"stripe_addon_prices_stripe_price_id_unique true UNIQUE (stripe_price_id)," +
			"subscription_items_team_id_teams_id_fk true FOREIGN KEY (team_id) REFERENCES teams(id) ON DELETE CASCADE," +
			"team_migrations_state_check true CHECK ((state = ANY (ARRAY['frozen'::text, 'moved'::text])))," +
			"team_migrations_team_id_teams_id_fk true FOREIGN KEY (team_id) REFERENCES teams(id)," +
			"teams_stripe_customer_id_unique true UNIQUE (stripe_customer_id)," +
			"teams_stripe_subscription_id_unique true UNIQUE (stripe_subscription_id)," +
			"teams_subscription_plan_id_subscription_plans_id_fk true FOREIGN KEY (subscription_plan_id) REFERENCES subscription_plans(id)",
		"default plan": "t",
	}
	for name, query := range checks {
		if got := p.sql(query); got != want[name] {
			p.t.Errorf("%s:\n got %s\nwant %s", name, got, want[name])
		}
	}
	p.t.Log("0008 verified: seven tables, two enums, four teams columns, named constraints validated, default plan present")
}

// verifyReferenceRows checks that every committed reference row is in the database with exactly the
// exported values; Stripe price IDs are the stack's, so they are compared apart.
func (p *controlDBProof) verifyReferenceRows() {
	p.t.Helper()
	for table, query := range map[string]string{
		"subscription_plans":  "SELECT jsonb_agg(to_jsonb(r) - 'stripe_price_id_monthly' - 'stripe_price_id_yearly' ORDER BY id) FROM subscription_plans r",
		"llms":                "SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM llms r",
		"stripe_addon_prices": "SELECT coalesce(jsonb_agg(to_jsonb(r) - 'stripe_price_id' ORDER BY id), '[]') FROM stripe_addon_prices r",
	} {
		want := "(SELECT jsonb_agg(e ORDER BY e->>'id') FROM jsonb_array_elements(" + deploymentSQLQuote(referenceRowJSON(p.t, table)) + "::jsonb) e"
		if table == "stripe_addon_prices" {
			// Only the rows this stack prices are seeded.
			want += " WHERE e->>'id' IN (SELECT id::text FROM stripe_addon_prices)"
			want = "coalesce(" + want + "), '[]')"
		} else {
			want += ")"
		}
		if got := p.sql("SELECT (" + query + ") = " + want); got != "t" {
			p.t.Errorf("%s rows differ from dashboard-reference-rows.sql", table)
		}
	}
	p.t.Log(p.sql("SELECT format('reference rows match the export: %s plans, %s llms, %s add-on prices', (SELECT count(*) FROM subscription_plans), (SELECT count(*) FROM llms), (SELECT count(*) FROM stripe_addon_prices))"))
}

// verifyTeamCreation is the dashboard's sign-up insert: a user and a team with the default plan,
// rolled back so the fixture stays as it was.
func (p *controlDBProof) verifyTeamCreation() {
	p.t.Helper()
	got := p.sqlScript(`BEGIN;
INSERT INTO users (id, first_name, email, firebase_id)
VALUES ('66666666-6666-4666-8666-666666666666', 'Schema fixture', 'schema-fixture@example.invalid', 'schema-fixture-378');
INSERT INTO teams (name, created_by) VALUES ('Schema fixture', '66666666-6666-4666-8666-666666666666') RETURNING subscription_plan_id;
ROLLBACK;`)
	if got != "0d735744-e58e-4438-b618-97523e407cdc" {
		p.t.Fatalf("team creation returned %q", got)
	}
	p.t.Log("a new team was created with the default plan 0d735744-e58e-4438-b618-97523e407cdc")
}
