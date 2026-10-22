package controller

import (
	"context"
	_ "embed"
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	lakeRuntimeRole              = "lake_runtime"
	lakeOperatorRole             = "lake_operator"
	lakeRuntimeCredentialHashKey = "orama.cloud/control-db-credential-hash"
)

//go:embed assets/lake-control-db-grants.sql
var lakeControlDBGrantsSQL string

func lakeRoleSecret(name, role string) string {
	return lakeControlDB(name) + "-" + strings.ReplaceAll(role, "_", "-")
}

// CNPG owns the login attributes and passwords; the migration owner alone applies grants.
func lakeManagedRoles(lake *oramav1alpha1.Lake) []interface{} {
	roles := make([]interface{}, 0, 2)
	for _, role := range []string{lakeRuntimeRole, lakeOperatorRole} {
		roles = append(roles, map[string]interface{}{
			"name": role, "ensure": "present", "login": true,
			"superuser": false, "createdb": false, "createrole": false,
			"replication": false, "bypassrls": false, "inherit": false,
			"connectionLimit": int64(40),
			"passwordSecret":  map[string]interface{}{"name": lakeRoleSecret(lake.Name, role)},
		})
	}
	return roles
}

func (r *LakeReconciler) reconcileLakeRoleSecrets(ctx context.Context, lake *oramav1alpha1.Lake, ns string, labels map[string]string) error {
	for _, role := range []string{lakeRuntimeRole, lakeOperatorRole} {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: lakeRoleSecret(lake.Name, role), Namespace: ns}}
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
			if secret.ResourceVersion != "" && !metav1.IsControlledBy(secret, lake) {
				return fmt.Errorf("control DB role Secret %s is not owned by this Lake", secret.Name)
			}
			if len(secret.Data["password"]) == 0 {
				if secret.ResourceVersion != "" {
					return fmt.Errorf("control DB role Secret %s has no password", secret.Name)
				}
				password, err := randHex(24)
				if err != nil {
					return err
				}
				secret.Data = map[string][]byte{"password": []byte(password)}
			}
			secret.Type = corev1.SecretTypeBasicAuth
			secret.Labels = withLabels(labels, map[string]string{"cnpg.io/reload": "true"})
			secret.Data["username"] = []byte(role)
			secret.Data["uri"] = []byte(fmt.Sprintf("postgresql://%s@%s-rw.%s:5432/%s", url.UserPassword(role, string(secret.Data["password"])).String(), lakeControlDB(lake.Name), ns, lakeControlDBName))
			return controllerutil.SetControllerReference(lake, secret, r.Scheme)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func lakeControlDBGrantsVersion() string {
	return schemaVersion(lakeSchemaVersion() + "\n" + lakeControlDBGrantsSQL)
}

// Grants run only after the complete pinned schema; a new schema also reapplies the grants.
func (r *LakeReconciler) reconcileLakeControlDBGrants(ctx context.Context, lake *oramav1alpha1.Lake, ns string, labels map[string]string) error {
	name := lakeControlDB(lake.Name) + "-grants"
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	ps := &oramav1alpha1.PostgresSchema{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if !lakePostgresEnabled(lake) {
		if err := deleteIfExists(ctx, r.Client, ps); err != nil {
			return err
		}
		return deleteIfExists(ctx, r.Client, cm)
	}
	if _, _, status := r.lakeControlDBSchemaStatus(ctx, lake, ns); status.phase != "Ready" {
		return nil
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = labels
		cm.Data = map[string]string{"grants.sql": lakeControlDBGrantsSQL}
		return controllerutil.SetControllerReference(lake, cm, r.Scheme)
	}); err != nil {
		return err
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ps, func() error {
		ps.Labels = withLabels(labels, lakeInstancePodLabels(lake))
		ps.Spec = oramav1alpha1.PostgresSchemaSpec{
			ConnectionSecretRef: oramav1alpha1.PostgresSecretKeyRef{Name: lakeControlDB(lake.Name) + "-app", Key: "uri"},
			SchemaConfigMapRef:  oramav1alpha1.PostgresConfigMapKeyRef{Name: name, Key: "grants.sql"},
			Version:             lakeControlDBGrantsVersion(), ApplyEveryVersion: true, Image: "postgres:18-alpine",
		}
		return controllerutil.SetControllerReference(lake, ps, r.Scheme)
	})
	return err
}

// No fallback to the owner: missing or stale grants hold both rollout and every row reconciler.
func (r *LakeReconciler) lakeSchemaStatus(ctx context.Context, lake *oramav1alpha1.Lake, ns string) (string, string, lakeComponentReadiness) {
	phase, version, state := r.lakeControlDBSchemaStatus(ctx, lake, ns)
	if !lakePostgresEnabled(lake) || state.phase != "Ready" {
		return phase, version, state
	}
	var grants oramav1alpha1.PostgresSchema
	err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: lakeControlDB(lake.Name) + "-grants"}, &grants)
	if err != nil {
		message := "waiting for the control DB grants"
		if !apierrors.IsNotFound(err) {
			message = "cannot read the control DB grants status"
		}
		return "Pending", version, lakeComponentReadiness{"Progressing", "GrantsPending", message}
	}
	if grants.Status.Phase == "Failed" && grants.Status.ObservedGeneration == grants.Generation {
		return "Failed", version, lakeComponentReadiness{"Degraded", "GrantsFailed", "control DB grants failed; inspect the migration Job"}
	}
	if grants.Status.Phase != "Applied" || grants.Status.ObservedGeneration != grants.Generation ||
		grants.Spec.Version != lakeControlDBGrantsVersion() || grants.Status.AppliedVersion != grants.Spec.Version ||
		!apimeta.IsStatusConditionTrue(grants.Status.Conditions, "Applied") {
		return "Applying", version, lakeComponentReadiness{"Progressing", "GrantsPending", "waiting for the current control DB grants to be Applied"}
	}
	return phase, version, state
}
