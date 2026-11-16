// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// dashboardSchemaSQL is the control-plane Postgres schema — the dashboard's Drizzle migrations
// (0000..0007, journal order) vendored from oramasearch/dashboard .drizzle. The operator applies it
// because the dashboard image ships no .drizzle and nothing else runs the migrations (operator#34).
// It is applied by `<cr>-controldb-schema`, guarded on crawler_configs. Never edit it to add a later
// migration: a database that already has crawler_configs skips it, so a later migration goes in its
// own file and its own PostgresSchema (dashboardSchema0008SQL is the first).
//
//go:embed assets/dashboard-schema.sql
var dashboardSchemaSQL string

// dashboardSchema0008SQL is the dashboard's Drizzle migration 0008_bouncy_ravenous, vendored from
// oramasearch/dashboard branch cloud-migration at commit 8e8f9a661f8f23461c5e5a2ded94e9d84e0a67b6
// (dashboard#112, unmerged; main and dev stop at 0007). It creates the tables the dashboard reads
// that 0000..0007 never created (subscription_plans, team_migrations, …) and the teams billing
// columns. `<cr>-controldb-schema-0008` applies it, guarded on team_migrations, only once
// `<cr>-controldb-schema` has applied its current version (operator#378).
//
//go:embed assets/dashboard-schema-0008.sql
var dashboardSchema0008SQL string

// dashboardReferenceDataSQL is the operator's upsert of the dashboard's global reference rows
// (subscription_plans, stripe_addon_prices, llms; operator#378). Its rows are the stack's
// spec.components.controlDB.referenceRows, or dashboardReferenceRowsSQL when that is empty, spliced
// in at referenceRowsMarker. controlDBReferenceSQL renders the two together with one stack's Stripe
// prices.
//
//go:embed assets/dashboard-reference-data.sql
var dashboardReferenceDataSQL string

// dashboardReferenceRowsSQL is the built-in example catalog: made-up rows, seeded by a stack whose
// spec.components.controlDB.referenceRows is empty. A stack's own catalog is configuration, in its
// spec, never in this repository.
//
//go:embed assets/dashboard-reference-rows.sql
var dashboardReferenceRowsSQL string

// referenceRowLine is the one statement shape a reference-rows line may have: one row's JSON, as a
// single-quoted SQL literal (a quote inside it doubled), staged into one of the three reference
// tables. The table names must match, so a line cannot stage rows into another table.
var referenceRowLine = regexp.MustCompile(`^INSERT INTO reference_(subscription_plans|stripe_addon_prices|llms) SELECT \* FROM json_populate_record\(NULL::reference_(subscription_plans|stripe_addon_prices|llms), '((?:[^']|'')*)'\);$`)

// controlDBReferenceRows is the stack's reference rows: spec.components.controlDB.referenceRows, or
// the built-in example catalog when it is empty. Every line must be a comment, blank, or one
// referenceRowLine statement whose literal is a JSON object; anything else is refused, so the field
// can only ever stage rows and never runs other SQL.
func controlDBReferenceRows(oc *oramav1alpha1.OramaCloudV1) (string, error) {
	rows := oc.Spec.Components.ControlDB.ReferenceRows
	if rows == "" {
		return dashboardReferenceRowsSQL, nil
	}
	for i, line := range strings.Split(rows, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		m := referenceRowLine.FindStringSubmatch(line)
		if m == nil || m[1] != m[2] {
			return "", fmt.Errorf("spec.components.controlDB.referenceRows line %d is not a reference row statement", i+1)
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(strings.ReplaceAll(m[3], "''", "'")), &row); err != nil {
			return "", fmt.Errorf("spec.components.controlDB.referenceRows line %d: the row is not a JSON object: %w", i+1, err)
		}
	}
	return rows, nil
}

const (
	referenceRowsMarker = "\n-- @reference-rows\n"
	// schema0008SeedPoint is the first 0008 statement after its tables: the reference rows go in
	// before it, so they exist before 0008 adds teams.subscription_plan_id and its foreign key.
	schema0008SeedPoint = `ALTER TABLE "teams" ADD COLUMN IF NOT EXISTS "subscription_plan_id"`
	seedBeginMarker     = "-- ============ operator#378: reference rows (dashboard-reference-data.sql), not part of 0008 ============\n"
	seedEndMarker       = "-- ============ end of operator#378 reference rows; 0008_bouncy_ravenous.sql continues ============\n--> statement-breakpoint\n"
)

// controlDBReferenceSQL renders the reference-row seed for one stack: temporary tables holding the
// stack's Stripe prices, then dashboard-reference-data.sql with rows spliced in. It fails when the
// embedded file lacks referenceRowsMarker.
func controlDBReferenceSQL(rows string, prices oramav1alpha1.ControlDBStripePrices) (string, error) {
	nullable := func(v string) string {
		if v == "" {
			return "NULL"
		}
		return deploymentSQLQuote(v)
	}
	var b strings.Builder
	b.WriteString("-- Stripe prices of this stack, from spec.components.controlDB.stripePrices (rendered by the operator).\n")
	b.WriteString("CREATE TEMP TABLE stack_stripe_plan_prices (id uuid PRIMARY KEY, monthly varchar(255), yearly varchar(255)) ON COMMIT DROP;\n")
	for _, p := range prices.Plans {
		fmt.Fprintf(&b, "INSERT INTO stack_stripe_plan_prices VALUES (%s, %s, %s);\n", deploymentSQLQuote(p.ID), nullable(p.Monthly), nullable(p.Yearly))
	}
	b.WriteString("CREATE TEMP TABLE stack_stripe_addon_prices (id uuid PRIMARY KEY, stripe_price_id varchar(255) NOT NULL) ON COMMIT DROP;\n")
	for _, a := range prices.Addons {
		fmt.Fprintf(&b, "INSERT INTO stack_stripe_addon_prices VALUES (%s, %s);\n", deploymentSQLQuote(a.ID), deploymentSQLQuote(a.PriceID))
	}
	before, after, found := strings.Cut(dashboardReferenceDataSQL, referenceRowsMarker)
	if !found {
		return "", fmt.Errorf("dashboard-reference-data.sql has no %q marker", strings.TrimSpace(referenceRowsMarker))
	}
	return b.String() + before + "\n" + rows + "\n" + after, nil
}

// controlDBSchema0008SQL is 0008 with the stack's reference rows seeded at schema0008SeedPoint,
// without any Stripe prices, so it is the same on every stack with the same rows. The vendored
// statements are unchanged; the operator's block sits between seedBeginMarker and seedEndMarker. It
// fails when the vendored 0008 no longer contains schema0008SeedPoint, so a re-vendored file cannot
// silently lose the seed.
func controlDBSchema0008SQL(rows string) (string, error) {
	head, tail, found := strings.Cut(dashboardSchema0008SQL, schema0008SeedPoint)
	if !found {
		return "", fmt.Errorf("dashboard-schema-0008.sql has no seed point %q", schema0008SeedPoint)
	}
	seed, err := controlDBReferenceSQL(rows, oramav1alpha1.ControlDBStripePrices{})
	if err != nil {
		return "", err
	}
	return head + seedBeginMarker + seed + seedEndMarker + schema0008SeedPoint + tail, nil
}

// cnpgClusterGVK is the CloudNativePG Cluster CRD. We provision it as an unstructured
// object so this operator stays decoupled from CNPG's Go types / version.
var cnpgClusterGVK = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}

const controlDBName = "oramacore" // database + owner

// controlDBAppSecret is the Secret CloudNativePG generates for the control-DB owner — it holds
// the REAL generated password + a ready-made connection `uri` (keys: password, uri, user, host,
// port, dbname, …). Postgres clients must source creds from HERE, not the operator's dummy
// secret, or their queries fail with the wrong password (#32).
func controlDBAppSecret(oc *oramav1alpha1.OramaCloudV1) string { return oc.Name + "-controldb-app" }

// pgPasswordEnv sources the CNPG-generated owner password into env var `name` (#32).
func pgPasswordEnv(oc *oramav1alpha1.OramaCloudV1, name string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: controlDBAppSecret(oc)}, Key: "password"}}}
}

// pgURIEnv sources the CNPG-generated connection URI (postgresql://owner:pw@<cr>-controldb-rw:5432/db)
// into env var `name` (#32) — avoids hand-building a URL around a placeholder password.
func pgURIEnv(oc *oramav1alpha1.OramaCloudV1, name string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: controlDBAppSecret(oc)}, Key: "uri"}}}
}

// reconcileControlDB provisions the always in-cluster control-plane Postgres as a
// CloudNativePG Cluster. On the primary it bootstraps a fresh cluster; on a read tier
// with replicate=true it stands up a replica cluster streaming from the primary peer.
// On the primary it applies the dashboard schema, then inserts the stack's oramacore_clusters row
// when the table is empty (controldb_cluster_row.go). Every stack declares the migration role
// (migrationManagedRole), which signs in only while an OramaCloudV1Migration targets the stack.
// Requires the CloudNativePG operator to be installed (see helmfile).
func (r *OramaCloudV1Reconciler) reconcileControlDB(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	db := oc.Spec.Components.ControlDB
	if db.Mode == "external" {
		return fmt.Errorf("controlDB mode=external is not supported: the control DB is always in-cluster")
	}
	if !db.Enabled {
		return r.teardownControlDB(ctx, oc) // disable ≠ leave running (#23)
	}

	// The migration role signs in only while a migration targets this stack.
	migrations, err := targetingMigrations(ctx, r.Client, oc)
	if err != nil {
		return err
	}
	if err := r.ensureMigrationRoleSecret(ctx, oc); err != nil {
		return err
	}
	if err := r.ensureDashboardRoleSecret(ctx, oc); err != nil {
		return err
	}

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	cluster.SetName(oc.Name + "-controldb")
	cluster.SetNamespace(oc.Namespace)

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, cluster, func() error {
		cluster.SetLabels(labelsFor(oc, "controldb"))
		spec := map[string]interface{}{
			"instances": int64(profileInstances(oc)),
			"storage":   controlDBStorage(db.Storage),
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{string(corev1.ResourceEphemeralStorage): v1EphemeralRequest},
				"limits":   map[string]interface{}{string(corev1.ResourceEphemeralStorage): v1EphemeralLimit},
			},
			"managed": map[string]interface{}{"roles": []interface{}{
				migrationManagedRole(oc, len(migrations) > 0), dashboardManagedRole(oc)}},
		}
		if image := oc.Spec.Images.Overrides["controldb"]; image != "" {
			spec["imageName"] = image
		}
		// The storage pass owns changes to existing storage; ordinary spec rendering
		// must retain the class, grown size, and any other CNPG storage keys.
		if cluster.GetResourceVersion() != "" {
			storage, found, err := unstructured.NestedMap(cluster.Object, "spec", "storage")
			if err != nil {
				return err
			}
			delete(spec, "storage")
			if found {
				spec["storage"] = storage
			}
		}
		if oc.Spec.Region.Role == "read" && db.Replicate {
			// Replica cluster: stream from the primary region's Postgres endpoint.
			// Credentials wiring (the streaming_replica secret) is a follow-up.
			host, port := splitHostPort(primaryPostgresEndpoint(oc), "5432")
			spec["replica"] = map[string]interface{}{"enabled": true, "source": "primary"}
			spec["externalClusters"] = []interface{}{
				map[string]interface{}{
					"name": "primary",
					"connectionParameters": map[string]interface{}{
						"host":   host,
						"port":   port,
						"dbname": controlDBName,
						"user":   "streaming_replica",
					},
				},
			}
		} else {
			spec["bootstrap"] = map[string]interface{}{
				"initdb": map[string]interface{}{"database": controlDBName, "owner": controlDBName},
			}
		}
		cluster.Object["spec"] = spec
		// Cluster metadata alone is not inherited by CNPG instance and bootstrap Job pods.
		if err := unstructured.SetNestedStringMap(cluster.Object, labelsFor(oc, "controldb"), "spec", "inheritedMetadata", "labels"); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(oc, cluster, r.Scheme)
	})
	if err != nil {
		return err
	}
	// Apply the dashboard schema on the PRIMARY (read-tier replicas stream it) — operator#34.
	if oc.Spec.Region.Role == "read" && db.Replicate {
		return nil
	}
	if err := r.reconcileControlDBMigrations(ctx, oc); err != nil {
		return err
	}
	// Once the schema is applied, insert the stack's c1 oramacore_clusters row into an empty table (#443).
	return r.reconcileControlDBClusterRow(ctx, oc)
}

// migrationManagedRole is the CNPG managed role the migration tool signs in as (principle 4): on
// every stack, never a superuser, at most migrationRoleConnectionLimit sessions, its password from
// migrationRoleSecretName. login follows whether an OramaCloudV1Migration targets the stack. Every
// field CNPG defaults is set, so a steady stack renders what the API stores. Its grants are the
// last control DB schema (migrationGrantsSQL).
func migrationManagedRole(oc *oramav1alpha1.OramaCloudV1, login bool) map[string]interface{} {
	return map[string]interface{}{
		"name":            migrationRole,
		"comment":         "OramaCloudV1Migration copies from Prod 1 (operator#653)",
		"ensure":          "present",
		"login":           login,
		"superuser":       false,
		"createdb":        false,
		"createrole":      false,
		"replication":     false,
		"bypassrls":       false,
		"inherit":         true,
		"connectionLimit": int64(migrationRoleConnectionLimit),
		"passwordSecret":  map[string]interface{}{"name": migrationRoleSecretName(oc)},
	}
}

// ensureMigrationRoleSecret creates <cr>-controldb-migration once, with a random 48-character hex
// password: username and password for CNPG, which sets the role's password from them (cnpg.io/reload
// makes it watch the Secret), and uri, the tool's connection to the primary. It is never changed.
func (r *OramaCloudV1Reconciler) ensureMigrationRoleSecret(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	pass, err := randHex(24)
	if err != nil {
		return fmt.Errorf("generate %s: %w", migrationRoleSecretName(oc), err)
	}
	lbls := labelsFor(oc, "controldb")
	lbls["cnpg.io/reload"] = "true"
	uri := fmt.Sprintf("postgresql://%s:%s@%s-controldb-rw.%s:5432/%s", migrationRole, pass, oc.Name, oc.Namespace, controlDBName)
	return r.createSecretIfAbsent(ctx, oc, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: migrationRoleSecretName(oc), Namespace: oc.Namespace, Labels: lbls},
		Type:       corev1.SecretTypeBasicAuth,
		Data:       map[string][]byte{"username": []byte(migrationRole), "password": []byte(pass), "uri": []byte(uri)},
	})
}

// postgresClientImageKey is the spec.images.overrides key of the psql-carrying image that the control
// DB's PostgresSchema Job and the cluster-row Job run. Unset, both run defaultPostgresClientImage.
const postgresClientImageKey = "postgres-client"

// defaultPostgresClientImage pins the upstream psql helper. Production may override its mirror.
const defaultPostgresClientImage = pinnedPostgres16Image

// postgresClientImage resolves the control DB's psql image like the data stores (storeImage): the
// postgres-client override wins, else the upstream default.
func postgresClientImage(oc *oramav1alpha1.OramaCloudV1) string {
	return storeImage(oc, postgresClientImageKey, defaultPostgresClientImage)
}

// controlDBSchema is one of the control DB's ordered PostgresSchemas: its name suffix, its key in
// the `<cr>-controldb-migrations` ConfigMap, its SQL and the table whose presence means it applied,
// or everyVersion for SQL applied again whenever it changes.
type controlDBSchema struct {
	suffix, key, sql, guard string
	everyVersion            bool
}

// controlDBSchemaSuffixes names every control DB PostgresSchema, in apply order. controlDBSchemas
// declares exactly these, and the network sandbox admits each one's migration Job to the control DB
// (operator#625), so a schema added here is reachable from its first run.
var controlDBSchemaSuffixes = []string{
	"-controldb-schema",
	"-controldb-schema-0008",
	"-controldb-reference-data",
	"-controldb-migration-grants",
	dashboardGrantsSchemaSuffix,
}

// controlDBSchemas lists the control DB's schemas in apply order. Each is created only after every
// one before it reports its current version Applied, because each builds on the tables of the last.
func controlDBSchemas(oc *oramav1alpha1.OramaCloudV1) ([]controlDBSchema, error) {
	rows, err := controlDBReferenceRows(oc)
	if err != nil {
		return nil, err
	}
	schema0008, err := controlDBSchema0008SQL(rows)
	if err != nil {
		return nil, err
	}
	reference, err := controlDBReferenceSQL(rows, oc.Spec.Components.ControlDB.StripePrices)
	if err != nil {
		return nil, err
	}
	schemas := []controlDBSchema{
		// crawler_configs is created by migration 0000; with --single-transaction it exists iff the
		// whole file applied.
		{suffix: controlDBSchemaSuffixes[0], key: "schema.sql", sql: dashboardSchemaSQL, guard: "crawler_configs"},
		// team_migrations is created by 0008, in the same transaction as the rest of 0008 and the
		// reference rows seeded inside it.
		{suffix: controlDBSchemaSuffixes[1], key: "schema-0008.sql", sql: schema0008, guard: "team_migrations"},
		// The same upsert with this stack's Stripe prices, applied again whenever the committed rows
		// or the prices change. Its guard is ignored (everyVersion); it names the table the upsert
		// needs, and is set so the API default never differs from what the operator writes.
		{suffix: controlDBSchemaSuffixes[2], key: "reference-data.sql", guard: "subscription_plans", everyVersion: true,
			sql: reference},
	}
	// The migration role's grants come last, after every table exists, and again whenever a schema
	// that creates tables changes (operator#653).
	// Then the dashboard role's grants (operator#99), on the same terms.
	return append(schemas,
		controlDBSchema{suffix: controlDBSchemaSuffixes[3], key: "migration-grants.sql",
			guard: "crawler_configs", everyVersion: true, sql: migrationGrantsSQL(schemas)},
		controlDBSchema{suffix: controlDBSchemaSuffixes[4], key: "dashboard-grants.sql",
			guard: "crawler_configs", everyVersion: true, sql: dashboardGrantsSQL(schemas)}), nil
}

// migrationGrantsSQL gives the migration role what the migration tool needs and nothing more
// (principle 4): reading and writing the rows of every table in schema public and using its
// sequences, never DDL, never another schema. The default privileges cover tables the owner creates
// later. Its header names the version of every schema before it that creates tables, so a new
// schema version changes this one's version and its tables are granted too.
func migrationGrantsSQL(before []controlDBSchema) string {
	var b strings.Builder
	b.WriteString(grantsHeader("The migration role's grants (operator#653)", before))
	role := migrationRole
	fmt.Fprintf(&b, "GRANT USAGE ON SCHEMA public TO %s;\n", role)
	fmt.Fprintf(&b, "GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %s;\n", role)
	fmt.Fprintf(&b, "GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %s;\n", role)
	// For the role that applies the schemas, the owner (<cr>-controldb-app), which creates every table.
	fmt.Fprintf(&b, "ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s;\n", role)
	fmt.Fprintf(&b, "ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO %s;\n", role)
	return b.String()
}

// reconcileControlDBMigrations applies the vendored dashboard schema (operator#34) as owned,
// reconciled PostgresSchema CRDs (operator#40) — the datastore-state-as-declarative-CRDs pattern.
// The operator renders each schema file into one ConfigMap and declares one PostgresSchema per file
// (controlDBSchemas), in order: the next file and its PostgresSchema are written only once the
// previous one is current (postgresSchemaCurrent), so 0008 never runs against a database without
// the tables it alters (operator#378). The PostgresSchema controller owns each idempotent migration Job (wait → guard →
// apply-in-one-txn) and records the applied version in its `.status`. A schema still outstanding is
// normal progress, not an error: the PostgresSchema events and the readiness requeue advance it.
func (r *OramaCloudV1Reconciler) reconcileControlDBMigrations(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	// Clean up the pre-CRD migration Job (superseded by the PostgresSchema-owned Job under a distinct
	// name) so an upgraded cluster doesn't leave dangling cruft. Safe — the schema is already applied
	// and the new Job's guard is idempotent.
	if err := deleteIfExists(ctx, r.Client, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: oc.Name + "-controldb-migrate", Namespace: oc.Namespace}}); err != nil {
		return err
	}

	cmName := oc.Name + "-controldb-migrations"
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: oc.Namespace}}
	schemas, err := controlDBSchemas(oc)
	if err != nil {
		return err
	}
	for _, s := range schemas {
		// A schema's SQL reaches the ConfigMap only when the schema is released, just before its
		// PostgresSchema, so no Job of a later schema can read content ahead of its turn.
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
			cm.Labels = labelsFor(oc, "controldb")
			if cm.Data == nil {
				cm.Data = map[string]string{}
			}
			cm.Data[s.key] = s.sql
			return controllerutil.SetControllerReference(oc, cm, r.Scheme)
		}); err != nil {
			return err
		}

		// The version is a content hash of the SQL so it changes exactly when the vendored file
		// does (auditable in `.status`).
		ps := &oramav1alpha1.PostgresSchema{ObjectMeta: metav1.ObjectMeta{Name: oc.Name + s.suffix, Namespace: oc.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ps, func() error {
			ps.Labels = labelsFor(oc, "controldb")
			ps.Spec = oramav1alpha1.PostgresSchemaSpec{
				ConnectionSecretRef: oramav1alpha1.PostgresSecretKeyRef{Name: controlDBAppSecret(oc), Key: "uri"},
				SchemaConfigMapRef:  oramav1alpha1.PostgresConfigMapKeyRef{Name: cmName, Key: s.key},
				Version:             schemaVersion(s.sql),
				GuardTable:          s.guard,
				ApplyEveryVersion:   s.everyVersion,
				Image:               postgresClientImage(oc),
				ImagePullSecrets:    imagePullSecrets(oc),
			}
			return controllerutil.SetControllerReference(oc, ps, r.Scheme)
		}); err != nil {
			return err
		}
		if err := r.retryFailedSchemaJob(ctx, ps); err != nil {
			return err
		}
		if !postgresSchemaCurrent(ps) {
			return nil
		}
	}
	return nil
}

// postgresSchemaCurrent reports whether a PostgresSchema's status says its CURRENT spec applied: an
// Applied phase for an older version or generation is stale and must not release the next schema.
func postgresSchemaCurrent(ps *oramav1alpha1.PostgresSchema) bool {
	return ps.Status.Phase == "Applied" &&
		ps.Status.AppliedVersion == ps.Spec.Version &&
		ps.Status.ObservedGeneration == ps.Generation
}

// schemaJobRetriedAnnotation records, on a control-DB PostgresSchema, the failed migration Job the
// operator last replaced and when, so the failure stays visible in the stack's status until the
// schema applies.
const schemaJobRetriedAnnotation = "orama.cloud/retried-failed-job"

// retryFailedSchemaJob recovers a control-DB schema whose migration Job exhausted its retries. The
// PostgresSchema controller never recreates a failed Job of the same version, so the stack's
// reconciler foreground-deletes it (UID and resourceVersion preconditions, so a newer Job is never
// hit) and the PostgresSchema controller creates a fresh one once its pods are gone. Each Job keeps
// its own 20-attempt backoff, so a SQL error that persists is retried at that pace, not in a loop.
// Only a terminally failed Job of the current version, controlled by this PostgresSchema, is
// touched: never an active, obsolete or unowned one. Lake schemas do not pass through here.
func (r *OramaCloudV1Reconciler) retryFailedSchemaJob(ctx context.Context, ps *oramav1alpha1.PostgresSchema) error {
	if postgresSchemaCurrent(ps) {
		if _, ok := ps.Annotations[schemaJobRetriedAnnotation]; !ok {
			return nil
		}
		patch := client.MergeFrom(ps.DeepCopy())
		delete(ps.Annotations, schemaJobRetriedAnnotation)
		return r.Patch(ctx, ps, patch)
	}
	job := &batchv1.Job{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ps.Namespace, Name: ps.Name + "-migrate"}, job); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(job, ps) || !job.DeletionTimestamp.IsZero() || !jobFailed(job) || job.Status.Active > 0 ||
		job.Annotations[pgSchemaVersionAnnotation] != ps.Spec.Version {
		return nil
	}
	patch := client.MergeFrom(ps.DeepCopy())
	if ps.Annotations == nil {
		ps.Annotations = map[string]string{}
	}
	ps.Annotations[schemaJobRetriedAnnotation] = fmt.Sprintf("%s failed; replaced %s", job.Name, time.Now().UTC().Format(time.RFC3339))
	if err := r.Patch(ctx, ps, patch); err != nil {
		return err
	}
	policy := metav1.DeletePropagationForeground
	return client.IgnoreNotFound(r.Delete(ctx, job, &client.DeleteOptions{
		PropagationPolicy: &policy,
		Preconditions:     &metav1.Preconditions{UID: &job.UID, ResourceVersion: &job.ResourceVersion},
	}))
}

// schemaVersion is a short content hash identifying a schema SQL, recorded in PostgresSchema status
// so the applied version changes exactly when the vendored schema does.
func schemaVersion(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

// profileInstances maps the deployment profile to a Postgres instance count.
func profileInstances(oc *oramav1alpha1.OramaCloudV1) int {
	switch oc.Spec.Profile {
	case "heavy":
		return 3
	case "medium":
		return 2
	default: // light
		return 1
	}
}

func primaryPostgresEndpoint(oc *oramav1alpha1.OramaCloudV1) string {
	for _, p := range oc.Spec.Peers {
		if p.PostgresEndpoint != "" {
			return p.PostgresEndpoint
		}
	}
	return ""
}

func splitHostPort(endpoint, defaultPort string) (string, string) {
	if endpoint == "" {
		return "", defaultPort
	}
	if i := strings.LastIndex(endpoint, ":"); i >= 0 {
		return endpoint[:i], endpoint[i+1:]
	}
	return endpoint, defaultPort
}
