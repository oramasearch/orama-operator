/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	lakeRehearsalRoleName = "lake_rehearsal"
	// lakeRehearsalLakeLabel names the Lake a rehearsal's scratch namespace and verifier pods
	// belong to. The Lake's network sandbox admits verifier pods carrying it to the control
	// database (allow-rehearsal-ingress-to-controldb).
	lakeRehearsalLakeLabel = "orama.cloud/rehearsal-lake"
	lakeRehearsalAppName   = "lake-postgres-rehearsal"
)

// lakeRehearsalPodLabels are the labels of a rehearsal verifier pod for one Lake.
func lakeRehearsalPodLabels(lake string) map[string]string {
	return map[string]string{"app.kubernetes.io/name": lakeRehearsalAppName, lakeRehearsalLakeLabel: lake}
}

func lakeRehearsalSecretName(lake *oramav1alpha1.Lake) string {
	return lakeControlDB(lake.Name) + "-rehearsal"
}

// CNPG maintains this read-only login on both source and recovered databases. Membership covers
// future tables too; it grants no DDL, writes, role administration, replication or RLS bypass.
func lakeRehearsalRole(secret string) map[string]interface{} {
	return map[string]interface{}{
		"name": lakeRehearsalRoleName, "ensure": "present", "login": true,
		"superuser": false, "createdb": false, "createrole": false,
		"replication": false, "bypassrls": false, "inherit": true,
		"connectionLimit": int64(4), "inRoles": []interface{}{"pg_read_all_data"},
		"passwordSecret": map[string]interface{}{"name": secret},
	}
}

// Mint once, preserve the password across retries and backups, and let CNPG apply it declaratively.
func (r *LakeReconciler) ensureLakeRehearsalSecret(ctx context.Context, lake *oramav1alpha1.Lake, ns string, labels map[string]string) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: lakeRehearsalSecretName(lake), Namespace: ns}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = withLabels(labels, map[string]string{"cnpg.io/reload": "true"})
		secret.Type = corev1.SecretTypeBasicAuth
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data["username"] = []byte(lakeRehearsalRoleName)
		if len(secret.Data["password"]) == 0 {
			password, err := randHex(24)
			if err != nil {
				return err
			}
			secret.Data["password"] = []byte(password)
		}
		return controllerutil.SetControllerReference(lake, secret, r.Scheme)
	})
	return err
}
