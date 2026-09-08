/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// A local delivery resource supplies a real same-namespace controller owner. The source
// ciphertext remains in the Lake namespace because its AAD is bound to that namespace.
func (r *LakeEnvironmentReconciler) reconcileEnvironmentDeliveryOwner(
	ctx context.Context,
	environment *oramav1alpha1.LakeEnvironment,
	namespace string,
	source *oramav1alpha1.LakeSealedSecret,
) (*oramav1alpha1.LakeSealedSecret, error) {
	var lake oramav1alpha1.Lake
	if err := r.Get(ctx, client.ObjectKey{Name: environment.Spec.LakeRef}, &lake); err != nil {
		return nil, err
	}
	owner := &oramav1alpha1.LakeSealedSecret{ObjectMeta: environmentObjectMeta(environment, source.Spec.Template.Name, namespace)}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, owner, func() error {
		if owner.ResourceVersion != "" && owner.Annotations[environmentOwnerAnnotation] != environmentOwner(environment) {
			return fmt.Errorf("LakeSealedSecret %s/%s is not owned by this environment", namespace, source.Spec.Template.Name)
		}
		if !owner.DeletionTimestamp.IsZero() {
			return fmt.Errorf("delivery owner %s/%s is being deleted", namespace, owner.Name)
		}
		owner.Labels = environmentLabels(environment)
		owner.Annotations = map[string]string{environmentOwnerAnnotation: environmentOwner(environment)}
		owner.Spec = oramav1alpha1.LakeSealedSecretSpec{
			LakeRef:  source.Spec.LakeRef,
			Template: source.Spec.Template,
			Source: oramav1alpha1.LakeSealedSecretSource{
				Type: "delivery",
				Delivery: &oramav1alpha1.LakeEnvironmentSecretDeliverySource{
					Environment: environment.Name,
					Name:        source.Name,
				},
			},
		}
		return controllerutil.SetControllerReference(&lake, owner, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile environment delivery owner: %w", err)
	}
	return owner, nil
}

func (r *LakeEnvironmentReconciler) deleteStaleEnvironmentDeliveryOwners(
	ctx context.Context,
	environment *oramav1alpha1.LakeEnvironment,
	namespace string,
	keep map[string]struct{},
) error {
	var owners oramav1alpha1.LakeSealedSecretList
	if err := r.List(ctx, &owners, client.InNamespace(namespace), client.MatchingLabels(environmentLabels(environment))); err != nil {
		return err
	}
	for i := range owners.Items {
		owner := &owners.Items[i]
		if owner.Annotations[environmentOwnerAnnotation] != environmentOwner(environment) {
			continue
		}
		if _, found := keep[owner.Spec.Template.Name]; found {
			continue
		}
		if err := r.Delete(ctx, owner); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// Delivery resources use the environment's reconciliation path so source validation,
// rotation, removal and status have one implementation. They never decrypt source ciphertext.
func (r *LakeSealedSecretReconciler) reconcileEnvironmentDelivery(
	ctx context.Context,
	resource *oramav1alpha1.LakeSealedSecret,
	lake *oramav1alpha1.Lake,
) (ctrl.Result, error) {
	if !resource.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	namespace := lake.Spec.Namespace
	if namespace == "" {
		namespace = "lake-dev"
	}
	delivery := resource.Spec.Source.Delivery
	key := client.ObjectKey{Namespace: namespace, Name: delivery.Environment}
	var environment oramav1alpha1.LakeEnvironment
	if err := r.Get(ctx, key, &environment); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return r.rejectEnvironmentDelivery(ctx, resource, "delivery environment no longer exists")
	}
	valid := environment.Spec.LakeRef == lake.Name &&
		resource.Namespace == lakeEnvironmentNamespace(&environment) &&
		resource.Name == resource.Spec.Template.Name &&
		resource.Annotations[environmentOwnerAnnotation] == environmentOwner(&environment) &&
		environment.DeletionTimestamp.IsZero()
	referenced := false
	for _, reference := range environment.Spec.SecretRefs {
		name, err := sealedSecretReferenceName(reference)
		if err == nil && name == delivery.Name {
			referenced = true
		}
	}
	if !valid || !referenced {
		return r.rejectEnvironmentDelivery(ctx, resource, "delivery is not authorized by its environment")
	}
	reconciler := &LakeEnvironmentReconciler{Client: r.Client, Scheme: r.Scheme, APIReader: r.APIReader}
	return reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
}

func (r *LakeSealedSecretReconciler) rejectEnvironmentDelivery(
	ctx context.Context,
	resource *oramav1alpha1.LakeSealedSecret,
	message string,
) (ctrl.Result, error) {
	var secrets corev1.SecretList
	if err := r.List(ctx, &secrets, client.InNamespace(resource.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		if !metav1.IsControlledBy(secret, resource) {
			continue
		}
		if err := r.Delete(ctx, secret); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
	}
	return r.fail(ctx, resource, "DeliveryInvalid", message)
}
