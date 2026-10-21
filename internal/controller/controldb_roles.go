/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The dashboard's own control DB role (#99, principle 4). The CloudNativePG owner role
// (<cr>-controldb-app) creates every table and is used by the PostgresSchema Jobs, the cluster-row
// Job and pgAdmin; the dashboard signs in as dashboardRole, a CNPG managed role with no DDL, no
// TRUNCATE and no other schema, and with read-only access to the reference rows the operator owns.

const (
	// dashboardRole is the dashboard's control DB role.
	dashboardRole = "dashboard"
	// dashboardRoleConnectionLimit bounds the role's sessions: the dashboard's postgres-js client
	// pools at most 10 per pod, so this leaves room for four pods, and keeps the dashboard from taking
	// every one of the server's default 100 connections from the owner's Jobs.
	dashboardRoleConnectionLimit = 40
)

// dashboardReadOnlyTables are the tables whose rows the operator owns: the reference rows the
// reference-data schema upserts (subscription_plans, stripe_addon_prices, llms) and the c1 row the
// cluster-row Job inserts (oramacore_clusters). The dashboard reads them and never writes them.
var dashboardReadOnlyTables = []string{"subscription_plans", "stripe_addon_prices", "llms", "oramacore_clusters"}

// dashboardRoleSecretName is <cr>-controldb-dashboard, the dashboard role's password Secret: username
// and password for CNPG, which sets the role's password from them, and uri, the dashboard's
// DATABASE_URL.
func dashboardRoleSecretName(oc *oramav1alpha1.OramaCloudV1) string {
	return oc.Name + "-controldb-dashboard"
}

// dashboardGrantsSchemaSuffix names the PostgresSchema that applies dashboardGrantsSQL.
const dashboardGrantsSchemaSuffix = "-controldb-dashboard-grants"

// dashboardSignsInAsItsRole reports whether the dashboard uses dashboardRole: wherever the control DB
// runs and is a primary that applies the schemas. A read-tier replica streams the primary's roles
// and grants, and its dashboard keeps the owner's credentials until replica credentials are wired
// (the replica's streaming_replica follow-up in reconcileControlDB); a stack without a control DB has
// nothing to sign in to, and keeps the references it had.
func dashboardSignsInAsItsRole(oc *oramav1alpha1.OramaCloudV1) bool {
	db := oc.Spec.Components.ControlDB
	return db.Enabled && !(oc.Spec.Region.Role == "read" && db.Replicate)
}

// dashboardManagedRole is the CNPG managed role the dashboard signs in as. Every field CNPG defaults
// is set, so a steady stack renders what the API stores. Its grants are dashboardGrantsSQL.
func dashboardManagedRole(oc *oramav1alpha1.OramaCloudV1) map[string]interface{} {
	return map[string]interface{}{
		"name":            dashboardRole,
		"comment":         "the dashboard's control DB client (operator#99)",
		"ensure":          "present",
		"login":           true,
		"superuser":       false,
		"createdb":        false,
		"createrole":      false,
		"replication":     false,
		"bypassrls":       false,
		"inherit":         true,
		"connectionLimit": int64(dashboardRoleConnectionLimit),
		"passwordSecret":  map[string]interface{}{"name": dashboardRoleSecretName(oc)},
	}
}

// ensureDashboardRoleSecret creates <cr>-controldb-dashboard once, with a random 48-character hex
// password, a basic-auth Secret CNPG watches (cnpg.io/reload). It is never changed.
func (r *OramaCloudV1Reconciler) ensureDashboardRoleSecret(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	pass, err := randHex(24)
	if err != nil {
		return fmt.Errorf("generate %s: %w", dashboardRoleSecretName(oc), err)
	}
	lbls := labelsFor(oc, "controldb")
	lbls["cnpg.io/reload"] = "true"
	uri := fmt.Sprintf("postgresql://%s:%s@%s-controldb-rw.%s:5432/%s", dashboardRole, pass, oc.Name, oc.Namespace, controlDBName)
	return r.createSecretIfAbsent(ctx, oc, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: dashboardRoleSecretName(oc), Namespace: oc.Namespace, Labels: lbls},
		Type:       corev1.SecretTypeBasicAuth,
		Data:       map[string][]byte{"username": []byte(dashboardRole), "password": []byte(pass), "uri": []byte(uri)},
	})
}

// grantsHeader names the version of every schema before a grants file that creates tables, so a new
// schema version changes the grants' version and its tables are granted too.
func grantsHeader(title string, before []controlDBSchema) string {
	var b strings.Builder
	fmt.Fprintf(&b, "-- %s, applied after:\n", title)
	for _, s := range before {
		if !s.everyVersion {
			fmt.Fprintf(&b, "--   %s %s\n", strings.TrimPrefix(s.suffix, "-"), schemaVersion(s.sql))
		}
	}
	return b.String()
}

// dashboardGrantsSQL gives the dashboard role what the dashboard does and nothing more: connecting,
// the temporary table the projects delete trigger creates (capture_project_data_before_delete),
// reading and writing the rows of every table in schema public except the operator-owned
// dashboardReadOnlyTables, which it only reads, and using its sequences. No DDL, no TRUNCATE, no
// other schema (analytics-processor's, drizzle's). The default privileges cover tables the owner
// creates later; the read-only tables are named, so a new one is added here.
func dashboardGrantsSQL(before []controlDBSchema) string {
	var b strings.Builder
	b.WriteString(grantsHeader("The dashboard role's grants (operator#99)", before))
	role := dashboardRole
	// The database's name is the stack's; current_database() keeps the file the same everywhere.
	fmt.Fprintf(&b, "DO $$ BEGIN EXECUTE format('GRANT CONNECT, TEMPORARY ON DATABASE %%I TO %s', current_database()); END $$;\n", role)
	fmt.Fprintf(&b, "GRANT USAGE ON SCHEMA public TO %s;\n", role)
	fmt.Fprintf(&b, "GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %s;\n", role)
	fmt.Fprintf(&b, "REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON %s FROM %s;\n", strings.Join(dashboardReadOnlyTables, ", "), role)
	fmt.Fprintf(&b, "GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %s;\n", role)
	fmt.Fprintf(&b, "ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s;\n", role)
	fmt.Fprintf(&b, "ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO %s;\n", role)
	return b.String()
}

// dashboardRoleReady reports whether the dashboard may sign in as its role: the dashboard grants
// PostgresSchema applied its current version, which also means CNPG had created the role (a GRANT
// to a missing role fails the Job). Until then the dashboard is not written, so a running one keeps
// the owner's credentials and a fresh one waits for the schema it needs anyway.
func (r *OramaCloudV1Reconciler) dashboardRoleReady(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (bool, error) {
	if !dashboardSignsInAsItsRole(oc) {
		return true, nil
	}
	ps := &oramav1alpha1.PostgresSchema{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + dashboardGrantsSchemaSuffix}, ps); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	schemas, err := controlDBSchemas(oc)
	if err != nil {
		return false, err
	}
	// Current means the version this operator renders, not one an older operator applied.
	for _, s := range schemas {
		if s.suffix == dashboardGrantsSchemaSuffix && ps.Spec.Version != schemaVersion(s.sql) {
			return false, nil
		}
	}
	return postgresSchemaCurrent(ps), nil
}

// dashboardDBEnv is the dashboard's control DB credentials, DATABASE_URL and POSTGRES_PASSWORD: its
// own role's from <cr>-controldb-dashboard where it signs in as its role (dashboardSignsInAsItsRole),
// otherwise the owner's.
func dashboardDBEnv(oc *oramav1alpha1.OramaCloudV1) []corev1.EnvVar {
	if !dashboardSignsInAsItsRole(oc) {
		return []corev1.EnvVar{pgPasswordEnv(oc, "POSTGRES_PASSWORD"), pgURIEnv(oc, "DATABASE_URL")}
	}
	return []corev1.EnvVar{
		secretKeyEnv("POSTGRES_PASSWORD", dashboardRoleSecretName(oc), "password"),
		secretKeyEnv("DATABASE_URL", dashboardRoleSecretName(oc), "uri"),
	}
}

// dashboardDBSecrets is the control DB Secret the dashboard reads, for its credentials hash.
func dashboardDBSecrets(oc *oramav1alpha1.OramaCloudV1) []string {
	if !dashboardSignsInAsItsRole(oc) {
		return nil
	}
	return []string{dashboardRoleSecretName(oc)}
}
