/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

// reconcileIdP deploys an in-cluster identity provider when idp.provider=keycloak.
// none/firebase/clerk are external (nothing to deploy).
func (r *OramaCloudV1Reconciler) reconcileIdP(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	if oc.Spec.IDP.Provider != "keycloak" {
		return r.teardownByComponent(ctx, oc, "keycloak")
	}
	// KC_BOOTSTRAP_ADMIN_USERNAME / KC_HTTP_ENABLED / KC_PROXY_HEADERS come from the
	// shared ConfigMap and KC_BOOTSTRAP_ADMIN_PASSWORD from the shared Secret (envFrom).
	return r.reconcileApp(ctx, oc, appDef{
		component: "keycloak",
		image:     "quay.io/keycloak/keycloak:26.0",
		port:      8080,
		args:      []string{"start-dev"},
	})
}

// reconcileDemoSeed runs a one-shot Job that seeds a demo project+index so a fresh
// install answers queries. Jobs are ~immutable, so create-if-absent.
func (r *OramaCloudV1Reconciler) reconcileDemoSeed(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	if !oc.Spec.DemoSeed {
		return nil
	}
	name := oc.Name + "-demo-seed"
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: name}, &batchv1.Job{}); err == nil {
		return nil // already created
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	lbls := labelsFor(oc, "demo-seed")
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace, Labels: lbls},
		Spec: batchv1.JobSpec{
			BackoffLimit: int32Ptr(3),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbls},
				Spec: corev1.PodSpec{
					ImagePullSecrets: imagePullSecrets(oc),
					RestartPolicy:    corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{{
						Name:  "seed",
						Image: "curlimages/curl:8.10.1",
						// Placeholder: real seed creates a demo project + index + docs via
						// the dashboard/OramaCore API once the engine is Ready.
						Command: []string{"sh", "-c", "echo 'demo-seed placeholder: create a demo project+index+docs via the API'"},
					}},
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(oc, job, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, job)
}
