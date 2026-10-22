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

// lakeDeploySecret is read only by the deployment control plane; lake never receives it.
func lakeDeploySecret(name string) string { return "lake-" + name + "-deploy" }

// reconcileLakeDeployToken mints an independent deployment bearer once per Lake. The prefix
// distinguishes it from Lake's instance-admin bearer, which remains in its own Secret.
func (r *LakeReconciler) reconcileLakeDeployToken(ctx context.Context, lake *oramav1alpha1.Lake, ns string, labels map[string]string) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: lakeDeploySecret(lake.Name), Namespace: ns}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = labels
		if len(secret.Data["token"]) == 0 {
			token, err := randHex(32)
			if err != nil {
				return err
			}
			if secret.Data == nil {
				secret.Data = map[string][]byte{}
			}
			secret.Data["token"] = []byte("lake_deploy_" + token)
		}
		return controllerutil.SetControllerReference(lake, secret, r.Scheme)
	})
	return err
}
