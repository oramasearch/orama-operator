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

// index-builder's own control DB role (operator#822, principle 4). index-builder's only Postgres
// client is Prisma, used for large-index builds against four tables of its own schema
// (indexes_forced_to, google_cloud_clusters, google_cloud_clusters_machines,
// google_cloud_clusters_deployments; orama-cloud-v1-bridge index-manager/apps/index-builder
// prisma/schema.prisma). None of them is in the control DB, and nothing runs its Prisma migrations,
// so large-index builds are not carried over to Prod 2 (decided under Steve's 2026-11-14
// delegation, the recommendation on operator#822).
//
// So index-builder signs in as indexBuilderRole, a CNPG managed role holding no privilege in the
// control DB: a large-index build fails on a permission error instead of a missing table, and the
// owner credential (<cr>-controldb-app) leaves the pod. Its grants file only takes away what a
// privilege on schema public could give it, and proves the role exists (a REVOKE from a missing role
// fails the Job). What PUBLIC holds by default (CONNECT and TEMPORARY on the database, USAGE on
// schema public) stays, as for every role; no table or sequence is readable or writable.
//
// If large-index builds are ever carried over, their four tables go in a schema of their own,
// index_builder, applied by a PostgresSchema, and the role gets SELECT on the first three and SELECT,
// INSERT and UPDATE on google_cloud_clusters_deployments, in indexBuilderGrantsSQL.

const (
	// indexBuilderRole is index-builder's control DB role.
	indexBuilderRole = "index_builder"
	// indexBuilderRoleConnectionLimit bounds its sessions: Prisma's default pool is
	// num_cpus * 2 + 1 per pod, and index-builder runs one pod.
	indexBuilderRoleConnectionLimit = 10
	// indexBuilderGrantsSchemaSuffix names the PostgresSchema that applies indexBuilderGrantsSQL.
	indexBuilderGrantsSchemaSuffix = "-controldb-index-builder-grants"
)

// indexBuilderRoleSecretName is <cr>-controldb-index-builder: username and password for CNPG, which
// sets the role's password from them, and uri, index-builder's DATABASE_URL.
func indexBuilderRoleSecretName(oc *oramav1alpha1.OramaCloudV1) string {
	return oc.Name + "-controldb-index-builder"
}

// indexBuilderSignsInAsItsRole is where index-builder uses its own role: where the dashboard does
// (dashboardSignsInAsItsRole), a control DB primary that applies the schemas.
func indexBuilderSignsInAsItsRole(oc *oramav1alpha1.OramaCloudV1) bool {
	return dashboardSignsInAsItsRole(oc)
}

// indexBuilderManagedRole is the CNPG managed role index-builder signs in as. Every field CNPG
// defaults is set, so a steady stack renders what the API stores.
func indexBuilderManagedRole(oc *oramav1alpha1.OramaCloudV1) map[string]interface{} {
	return map[string]interface{}{
		"name":            indexBuilderRole,
		"comment":         "index-builder's control DB client, with no privileges (operator#822)",
		"ensure":          "present",
		"login":           true,
		"superuser":       false,
		"createdb":        false,
		"createrole":      false,
		"replication":     false,
		"bypassrls":       false,
		"inherit":         true,
		"connectionLimit": int64(indexBuilderRoleConnectionLimit),
		"passwordSecret":  map[string]interface{}{"name": indexBuilderRoleSecretName(oc)},
	}
}

// ensureIndexBuilderRoleSecret creates <cr>-controldb-index-builder once, with a random
// 48-character hex password, a basic-auth Secret CNPG watches (cnpg.io/reload). It is never changed.
func (r *OramaCloudV1Reconciler) ensureIndexBuilderRoleSecret(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	pass, err := randHex(24)
	if err != nil {
		return fmt.Errorf("generate %s: %w", indexBuilderRoleSecretName(oc), err)
	}
	lbls := labelsFor(oc, "controldb")
	lbls["cnpg.io/reload"] = "true"
	uri := fmt.Sprintf("postgresql://%s:%s@%s-controldb-rw.%s:5432/%s", indexBuilderRole, pass, oc.Name, oc.Namespace, controlDBName)
	return r.createSecretIfAbsent(ctx, oc, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: indexBuilderRoleSecretName(oc), Namespace: oc.Namespace, Labels: lbls},
		Type:       corev1.SecretTypeBasicAuth,
		Data:       map[string][]byte{"username": []byte(indexBuilderRole), "password": []byte(pass), "uri": []byte(uri)},
	})
}

// indexBuilderGrantsSQL leaves index-builder's role with no privilege on any table, sequence or on
// schema public itself, now and for tables the owner creates later. It grants nothing. Its header
// names the version of every schema before it that creates tables, so it runs again when one changes.
func indexBuilderGrantsSQL(before []controlDBSchema) string {
	var b strings.Builder
	b.WriteString(grantsHeader("index-builder's role: no privileges (operator#822)", before))
	role := indexBuilderRole
	fmt.Fprintf(&b, "REVOKE ALL ON ALL TABLES IN SCHEMA public FROM %s;\n", role)
	fmt.Fprintf(&b, "REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM %s;\n", role)
	fmt.Fprintf(&b, "REVOKE CREATE ON SCHEMA public FROM %s;\n", role)
	fmt.Fprintf(&b, "ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES FROM %s;\n", role)
	fmt.Fprintf(&b, "ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON SEQUENCES FROM %s;\n", role)
	return b.String()
}

// indexBuilderRoleReady reports whether index-builder may sign in as its role: its grants
// PostgresSchema applied the version this operator renders, which also means CNPG had created the
// role. Until then index-builder is not written, so a running one keeps the owner's credentials.
func (r *OramaCloudV1Reconciler) indexBuilderRoleReady(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (bool, error) {
	if !indexBuilderSignsInAsItsRole(oc) {
		return true, nil
	}
	ps := &oramav1alpha1.PostgresSchema{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + indexBuilderGrantsSchemaSuffix}, ps); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	schemas, err := controlDBSchemas(oc)
	if err != nil {
		return false, err
	}
	for _, s := range schemas {
		if s.suffix == indexBuilderGrantsSchemaSuffix && ps.Spec.Version != schemaVersion(s.sql) {
			return false, nil
		}
	}
	return postgresSchemaCurrent(ps), nil
}

// indexBuilderDBEnv is index-builder's DATABASE_URL: its own role's from <cr>-controldb-index-builder
// where it signs in as its role, otherwise the owner's.
func indexBuilderDBEnv(oc *oramav1alpha1.OramaCloudV1) corev1.EnvVar {
	if !indexBuilderSignsInAsItsRole(oc) {
		return pgURIEnv(oc, "DATABASE_URL")
	}
	return secretKeyEnv("DATABASE_URL", indexBuilderRoleSecretName(oc), "uri")
}

// indexBuilderDBSecret is the control DB Secret index-builder reads, for its credentials hash.
func indexBuilderDBSecret(oc *oramav1alpha1.OramaCloudV1) string {
	if !indexBuilderSignsInAsItsRole(oc) {
		return controlDBAppSecret(oc)
	}
	return indexBuilderRoleSecretName(oc)
}
